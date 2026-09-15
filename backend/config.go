package main

import (
	"bufio"
	"log"
	"os"
	"strconv"
	"strings"
)

// Config holds every runtime setting, sourced from environment variables
// (which .env populates on startup — see loadDotenv).
type Config struct {
	OpenAIKey  string
	ModelFast  string
	ModelSmart string
	OpenAIBase string

	// AILive is the explicit master switch for live ChatGPT generation.
	// Live mode requires BOTH this flag and a key, so a key sitting in .env can
	// be parked without being spent, and "am I live?" is a setting you can read
	// rather than a side effect of whether a variable happens to be blank.
	AILive bool

	Port       string
	CORSOrigin string

	DailyCallsPerUser int
	MaxConcurrency    int
	MonthlyUSDCap     float64

	DataDir     string
	FrontendDir string

	DefaultTimezone     string
	RolloverIntervalMin int

	// AllowHeaderAuth re-enables identifying a caller by a bare X-User-Id
	// header. It exists only for local demos and legacy clients: a header the
	// caller chooses is not authentication, so it defaults to off.
	AllowHeaderAuth bool
}

// loadDotenv reads KEY=VALUE lines from path into the process environment.
// Real environment variables always win, so it never clobbers an explicit setting.
func loadDotenv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
	if err := sc.Err(); err != nil {
		log.Printf("config: %s read error: %v", path, err)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
		log.Printf("config: %s=%q is not an integer; using %d", key, v, def)
	}
	return def
}

func getenvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return n
		}
		log.Printf("config: %s=%q is not a number; using %g", key, v, def)
	}
	return def
}

func getenvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
		log.Printf("config: %s=%q is not a boolean; using %v", key, v, def)
	}
	return def
}

func loadConfig() Config {
	loadDotenv(".env")
	cfg := Config{
		OpenAIKey:  os.Getenv("OPENAI_API_KEY"),
		ModelFast:  getenv("OPENAI_MODEL_FAST", "gpt-4o-mini"),
		ModelSmart: getenv("OPENAI_MODEL_SMART", "gpt-4o"),
		OpenAIBase: getenv("OPENAI_BASE_URL", "https://api.openai.com/v1"),
		AILive:     getenvBool("AI_LIVE", true),
		Port:       getenv("PORT", "8080"),
		// Empty by default: the server hosts its own frontend on the same
		// origin, so no cross-origin grant is needed out of the box.
		CORSOrigin:          getenv("CORS_ALLOW_ORIGIN", ""),
		DailyCallsPerUser:   getenvInt("AI_DAILY_CALLS_PER_USER", 40),
		MaxConcurrency:      getenvInt("AI_MAX_CONCURRENCY", 4),
		MonthlyUSDCap:       getenvFloat("AI_MONTHLY_USD_CAP", 50),
		DataDir:             getenv("DATA_DIR", "./data"),
		FrontendDir:         getenv("FRONTEND_DIR", "../frontend"),
		DefaultTimezone:     getenv("DEFAULT_TIMEZONE", "Asia/Tashkent"),
		RolloverIntervalMin: getenvInt("ROLLOVER_INTERVAL_MINUTES", 1440),
		AllowHeaderAuth:     getenvBool("ALLOW_HEADER_AUTH", false),
	}
	if cfg.CORSOrigin == "*" {
		log.Printf("config: CORS_ALLOW_ORIGIN=* lets any website call this API on a user's behalf; set a specific origin in production")
	}
	// Both halves of the switch are reported, because each mismatch is a
	// different mistake and both otherwise present as "why is it still mock?".
	switch {
	case cfg.AILive && cfg.OpenAIKey == "":
		log.Printf("config: AI_LIVE=true but OPENAI_API_KEY is empty; staying in MOCK mode")
	case !cfg.AILive && cfg.OpenAIKey != "":
		log.Printf("config: AI_LIVE=false overrides the OPENAI_API_KEY that is set; staying in MOCK mode")
	}
	if cfg.AllowHeaderAuth {
		log.Printf("config: ALLOW_HEADER_AUTH=true accepts an unverified X-User-Id header as identity; do not use this in production")
	}
	return cfg
}
