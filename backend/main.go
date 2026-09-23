package main

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	// Embedded zoneinfo so LoadLocation("Asia/Tashkent") works on hosts without
	// a system tzdata database (notably Windows and scratch containers).
	_ "time/tzdata"
)

func staticStat(dir string) (fs.FileInfo, error) { return os.Stat(dir) }

func main() {
	cfg := loadConfig()

	store := newStore(cfg.DataDir)
	gw := newGateway(cfg)
	sched := newScheduler(store)
	pipe := newPipeline(cfg, store, gw, sched)
	api := newAPI(cfg, store, gw, pipe, sched)

	rootCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// Nightly rollover job: in production this is a cron; here it's a ticker.
	// It sweeps every user, rolling missed sessions forward and shifting finish
	// dates, using each user's own timezone to decide what "today" means.
	go func() {
		interval := time.Duration(cfg.RolloverIntervalMin) * time.Minute
		if interval <= 0 {
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-ticker.C:
				for _, id := range store.UserIDs() {
					// "Today" is decided in the learner's zone, with the
					// configured default standing in only if the user record
					// has gone missing — never the host machine's zone.
					loc := loadLocation(cfg.DefaultTimezone)
					if u, ok := store.GetUser(id); ok {
						loc = loadLocation(u.Timezone)
					}
					sched.Rollover(id, todayIn(loc))
				}
			}
		}
	}()

	// Name the reason for mock mode: "no key" and "switched off" need different
	// fixes, and the banner is the first place anyone looks.
	mode := "MOCK (no OPENAI_API_KEY — realistic canned plans)"
	switch {
	case gw.Enabled():
		mode = "LIVE ChatGPT API (" + cfg.ModelFast + " / " + cfg.ModelSmart + ")"
	case !cfg.AILive:
		mode = "MOCK (AI_LIVE=false — realistic canned plans)"
	}
	log.Printf("start.ai backend listening on :%s", cfg.Port)
	log.Printf("AI mode: %s", mode)
	// Say plainly whether product lookups can happen. None ship with this
	// build, and a silent absence would read as "search is on but found
	// nothing" — which is how fabricated recommendations get believed.
	if names := pipe.res.ProviderNames(); len(names) > 0 {
		log.Printf("resource providers: %s", strings.Join(names, ", "))
	} else {
		log.Printf("resource providers: NONE configured — no verified prices or stock; plans carry shop SEARCH links only (Uzum, Yandex Market, Coursera, Udemy, Stepik)")
	}
	if cfg.FrontendDir != "" {
		if fi, err := staticStat(cfg.FrontendDir); err == nil && fi.IsDir() {
			log.Printf("serving frontend from %s → open http://localhost:%s", cfg.FrontendDir, cfg.Port)
		}
	}

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: api.routes(),
		// A server with only a header timeout can still be tied up indefinitely
		// by a slow body or an idle connection.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second, // must exceed the 55s AI call budget
		IdleTimeout:       120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			log.Printf("server error: %v", err)
		}
	case <-rootCtx.Done():
		log.Printf("shutting down…")
	}

	// Drain in-flight requests, then make sure the last state changes reach disk
	// instead of dying with the process.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	gw.Close()
	store.Close()
	log.Printf("stopped cleanly")
}
