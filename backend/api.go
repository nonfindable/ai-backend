package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"
)

type API struct {
	cfg   Config
	store *Store
	gw    *Gateway
	pipe  *Pipeline
	sched *Scheduler

	// Conversations and plans are each mutated by more than one endpoint. These
	// serialize work per entity, so a turn or a reschedule is atomic without
	// holding the store's global lock across an AI call.
	//
	// planLocks is NOT created here: it is shared with the Scheduler, so the
	// rollover endpoint and the nightly sweep contend on the same mutex as the
	// schedule/confirm/complete handlers. Two separate mutexes excluded nothing
	// and let a stale rollover write resurrect events a reschedule had deleted.
	// Lock order where both are taken: userLocks then planLocks.
	sessionLocks *keyedMutex
	planLocks    *keyedMutex
	userLocks    *keyedMutex
}

func newAPI(cfg Config, store *Store, gw *Gateway, pipe *Pipeline, sched *Scheduler) *API {
	return &API{
		cfg: cfg, store: store, gw: gw, pipe: pipe, sched: sched,
		sessionLocks: newKeyedMutex(),
		planLocks:    sched.planLocks, // shared: see the field comment
		userLocks:    newKeyedMutex(),
	}
}

// maxRequestBytes caps any request body. Without it a single POST could buffer
// an unbounded amount of memory.
const maxRequestBytes = 1 << 20 // 1 MiB

func (a *API) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })

	// Public: issues the identity every other route requires.
	mux.HandleFunc("POST /api/session", a.handleSession)

	// Authenticated.
	mux.Handle("POST /api/chat", a.requireUser(a.handleChat))
	mux.Handle("GET /api/plan/{id}", a.requireUser(a.handlePlan))
	mux.Handle("GET /api/plan/{id}/ics", a.requireUser(a.handleICS))
	mux.Handle("POST /api/schedule", a.requireUser(a.handleSchedule))
	mux.Handle("POST /api/schedule/confirm", a.requireUser(a.handleConfirm))
	mux.Handle("GET /api/calendar", a.requireUser(a.handleCalendar))
	mux.Handle("POST /api/todo/complete", a.requireUser(a.handleComplete))
	mux.Handle("POST /api/rollover", a.requireUser(a.handleRollover))
	mux.Handle("GET /api/meter", a.requireUser(a.handleMeter))

	var handler http.Handler = mux
	// Optionally serve the static frontend so `go run .` gives a one-command app.
	if a.cfg.FrontendDir != "" {
		if fi, err := staticStat(a.cfg.FrontendDir); err == nil && fi.IsDir() {
			root := http.NewServeMux()
			root.Handle("/api/", mux)
			root.Handle("/healthz", mux)
			root.Handle("/", http.FileServer(http.Dir(a.cfg.FrontendDir)))
			handler = root
		}
	}
	return a.withCORS(a.limitBody(handler))
}

// limitBody caps request bodies before any handler reads them.
func (a *API) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.CORSOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", a.cfg.CORSOrigin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-User-Id")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- authentication ----

// requireUser resolves the bearer token issued by POST /api/session. Every
// object lookup downstream is then checked against this identity — previously
// any caller who knew (or guessed) a plan ID could read and modify it.
func (a *API) requireUser(next func(http.ResponseWriter, *http.Request, *User)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := a.authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="start.ai"`)
			writeAPIError(w, http.StatusUnauthorized, codeUnauthorized)
			return
		}
		next(w, r, user)
	})
}

var errUnauthenticated = errors.New("unauthenticated")

func (a *API) authenticate(r *http.Request) (*User, error) {
	if tok := bearerToken(r); tok != "" {
		if u, ok := a.store.UserByToken(tok); ok {
			return u, nil
		}
		return nil, errUnauthenticated
	}
	// Opt-in escape hatch for local demos and existing clients that only send
	// an ID. Off by default because a self-asserted header is not authentication.
	if a.cfg.AllowHeaderAuth {
		if id := strings.TrimSpace(r.Header.Get("X-User-Id")); id != "" {
			if u, ok := a.store.GetUser(id); ok {
				return u, nil
			}
		}
	}
	return nil, errUnauthenticated
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// ownedPlan fetches a plan only if it belongs to user. It deliberately reports
// "not found" rather than "forbidden" so plan IDs cannot be probed.
func (a *API) ownedPlan(id string, user *User) (*Plan, bool) {
	plan, ok := a.store.GetPlan(id)
	if !ok || plan.UserID != user.ID {
		return nil, false
	}
	return plan, true
}

// ---- handlers ----

type sessionReq struct {
	Name     string `json:"name"`
	Timezone string `json:"timezone"`
	Lang     string `json:"lang"`
}

func (a *API) handleSession(w http.ResponseWriter, r *http.Request) {
	var req sessionReq
	_ = readJSON(r, &req)

	// An existing caller continues as themselves only by presenting their token;
	// a userId in the body or header is not proof of identity.
	user, err := a.authenticate(r)
	token := ""
	if err != nil {
		token = newToken()
		user = &User{
			ID:   newID("user"),
			Name: strings.TrimSpace(req.Name),
			// Resolved, not merely copied: an unknown IANA name stored here
			// would be silently reinterpreted as the server's zone every time
			// the scheduler touched this user's plans.
			Timezone:  resolveTimezone(req.Timezone, a.cfg.DefaultTimezone),
			CreatedAt: time.Now(),
		}
		a.store.SaveUser(user)
		a.store.SaveToken(token, user.ID)
	} else if tz := resolveTimezone(req.Timezone, user.Timezone); strings.TrimSpace(req.Timezone) != "" && tz != user.Timezone {
		user.Timezone = tz
		a.store.SaveUser(user)
	}

	lang := normLang(req.Lang)
	sess := &IntakeSession{
		ID: newID("sess"), UserID: user.ID, Stage: "scope_check", Lang: lang,
		AnswerBag: map[string]string{}, CreatedAt: time.Now(),
	}
	greeting := sessionGreeting(lang, req.Name)
	sess.Messages = append(sess.Messages, Message{Role: "assistant", Content: greeting, At: time.Now()})
	a.store.SaveSession(sess)

	resp := map[string]any{
		"userId": user.ID, "sessionId": sess.ID, "stage": sess.Stage,
		"assistant": greeting, "timezone": user.Timezone,
	}
	if token != "" {
		// Returned exactly once, at creation.
		resp["token"] = token
	}
	writeJSON(w, 200, resp)
}

// maxGreetingNameRunes caps the name echoed into the greeting, so an
// oversized name cannot swamp the first message.
const maxGreetingNameRunes = 40

// sessionGreeting is the opening assistant message, addressed by name when
// the request carried one. The name is inserted as plain text: clients render
// assistant messages as text, never as HTML.
func sessionGreeting(lang, name string) string {
	const (
		en = " 👋 I'm start.ai, your learning coach. What would you like to learn? Just tell me in your own words — I'll ask a few quick questions and build a step-by-step plan that fits your schedule."
		ru = " 👋 Я start.ai — ваш помощник в учёбе. Что хотите изучить? Просто напишите своими словами — я задам пару коротких вопросов и составлю пошаговый план под ваш график."
		uz = " 👋 Men start.ai — o'qishdagi yordamchingizman. Nimani o'rganmoqchisiz? O'z so'zlaringiz bilan yozing — men bir nechta qisqa savol beraman va vaqtingizga mos bosqichma-bosqich reja tuzib beraman."
	)
	name = greetingName(name)
	if name == "" {
		return tr(lang, "Hi there!"+en, "Привет!"+ru, "Salom!"+uz)
	}
	return tr(lang, "Hi "+name+"!"+en, "Привет, "+name+"!"+ru, "Salom, "+name+"!"+uz)
}

// greetingName trims the name, folds any line breaks or control characters
// into spaces so it stays on one line, and caps it at maxGreetingNameRunes.
func greetingName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	if r := []rune(name); len(r) > maxGreetingNameRunes {
		name = strings.TrimSpace(string(r[:maxGreetingNameRunes]))
	}
	return name
}

type chatReq struct {
	SessionID string `json:"sessionId"`
	Message   string `json:"message"`
	Lang      string `json:"lang"`
}

func (a *API) handleChat(w http.ResponseWriter, r *http.Request, user *User) {
	var req chatReq
	if err := readJSON(r, &req); err != nil {
		writeAPIError(w, 400, codeInvalidRequest)
		return
	}
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.Message) == "" {
		writeAPIError(w, 400, codeInvalidRequest)
		return
	}

	unlock := a.sessionLocks.Lock(req.SessionID)
	defer unlock()

	sess, ok := a.store.GetSession(req.SessionID)
	if !ok || sess.UserID != user.ID {
		writeAPIError(w, 404, codeSessionNotFound)
		return
	}
	if req.Lang != "" {
		sess.Lang = normLang(req.Lang) // let the user switch language mid-conversation
	}

	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()

	turn, err := a.pipe.HandleChat(ctx, sess, req.Message)
	if err != nil {
		// The cause is logged under a reference; the client gets a stable code
		// and a fixed message. Forwarding err.Error() here once leaked the
		// provider's billing text straight to callers.
		status, code := classifyAIError(err)
		writeAPIErrorLogging(w, status, code, "chat", err)
		return
	}
	writeJSON(w, 200, turn)
}

func (a *API) handlePlan(w http.ResponseWriter, r *http.Request, user *User) {
	plan, ok := a.ownedPlan(r.PathValue("id"), user)
	if !ok {
		writeAPIError(w, 404, codePlanNotFound)
		return
	}
	writeJSON(w, 200, withShopLinks(plan, a.cfg.MarketplaceCountry))
}

type planRef struct {
	PlanID string `json:"planId"`
}

func (a *API) handleSchedule(w http.ResponseWriter, r *http.Request, user *User) {
	var req planRef
	if err := readJSON(r, &req); err != nil {
		writeAPIError(w, 400, codeInvalidRequest)
		return
	}
	unlock := a.planLocks.Lock(req.PlanID)
	defer unlock()

	plan, ok := a.ownedPlan(req.PlanID, user)
	if !ok {
		writeAPIError(w, 404, codePlanNotFound)
		return
	}
	events := a.sched.Schedule(plan, a.store.EventsForPlan(plan.ID))
	a.store.ReplaceEventsForPlan(plan.ID, events)
	a.store.SavePlan(plan)
	writeJSON(w, 200, map[string]any{
		"planId": plan.ID, "startDate": plan.StartDate, "finishDate": plan.FinishDate,
		"events": events, "count": len(events),
		"droppedSessions": plan.DroppedSessions,
		"missesDeadline":  plan.MissesDeadline, "deadlineSlipDays": plan.DeadlineSlipDays,
		"deadlineNote": plan.DeadlineNote,
	})
}

func (a *API) handleConfirm(w http.ResponseWriter, r *http.Request, user *User) {
	var req planRef
	if err := readJSON(r, &req); err != nil {
		writeAPIError(w, 400, codeInvalidRequest)
		return
	}
	unlock := a.planLocks.Lock(req.PlanID)
	defer unlock()

	plan, ok := a.ownedPlan(req.PlanID, user)
	if !ok {
		writeAPIError(w, 404, codePlanNotFound)
		return
	}
	events := a.store.EventsForPlan(plan.ID)
	confirmed := 0
	for _, ev := range events {
		if ev.Status == "proposed" {
			ev.Status = "scheduled"
			confirmed++
		}
	}
	a.store.SaveEvents(events)
	writeJSON(w, 200, map[string]any{
		"planId": plan.ID, "confirmed": confirmed, "total": len(events),
		"finishDate": plan.FinishDate,
	})
}

func (a *API) handleCalendar(w http.ResponseWriter, r *http.Request, user *User) {
	if planID := strings.TrimSpace(r.URL.Query().Get("planId")); planID != "" {
		if _, ok := a.ownedPlan(planID, user); !ok {
			writeAPIError(w, 404, codePlanNotFound)
			return
		}
		writeJSON(w, 200, a.store.EventsForPlan(planID))
		return
	}
	writeJSON(w, 200, a.store.EventsForUser(user.ID))
}

type completeReq struct {
	PlanID  string `json:"planId"`
	TodoID  string `json:"todoId"`
	EventID string `json:"eventId"` // optional: complete a specific session
}

func (a *API) handleComplete(w http.ResponseWriter, r *http.Request, user *User) {
	var req completeReq
	if err := readJSON(r, &req); err != nil {
		writeAPIError(w, 400, codeInvalidRequest)
		return
	}
	unlock := a.planLocks.Lock(req.PlanID)
	defer unlock()

	plan, ok := a.ownedPlan(req.PlanID, user)
	if !ok {
		writeAPIError(w, 404, codePlanNotFound)
		return
	}

	idx := -1
	for i := range plan.Todos {
		if plan.Todos[i].ID == req.TodoID {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeAPIError(w, 404, codeTodoNotFound)
		return
	}
	t := &plan.Todos[idx]

	// Complete ONE session of this todo, not the whole series. Marking the todo
	// itself done used to cancel every remaining occurrence on the next
	// reschedule.
	events := a.store.EventsForPlan(plan.ID)
	var target *CalendarEvent
	for _, ev := range events {
		if ev.TodoID != req.TodoID || ev.Status == "done" {
			continue
		}
		if req.EventID != "" {
			if ev.ID == req.EventID {
				target = ev
				break
			}
			continue
		}
		target = ev // events are date-ordered, so this is the soonest outstanding
		break
	}
	if req.EventID != "" && target == nil {
		writeAPIError(w, 404, codeEventNotFound)
		return
	}
	if target != nil {
		target.Status = "done"
		a.store.SaveEvents(events)
	}

	if t.PlannedCount <= 0 {
		t.PlannedCount = 1
	}
	t.CompletedCount = minInt(t.CompletedCount+1, t.PlannedCount)
	now := time.Now()
	t.CompletedAt = &now
	syncTodoStatus(t)

	a.store.SavePlan(plan)
	a.store.AddProgress(&ProgressLog{
		ID: newID("log"), UserID: plan.UserID, PlanID: plan.ID, TodoID: req.TodoID,
		Event: "completed", At: now,
	})
	writeJSON(w, 200, map[string]any{
		"ok": true, "todoId": t.ID, "status": t.Status,
		"completedCount": t.CompletedCount, "plannedCount": t.PlannedCount,
		"remaining": t.Remaining(),
	})
}

type rolloverReq struct {
	AsOf string `json:"asOf"` // optional YYYY-MM-DD to simulate "today" in a demo
}

func (a *API) handleRollover(w http.ResponseWriter, r *http.Request, user *User) {
	var req rolloverReq
	_ = readJSON(r, &req)

	unlock := a.userLocks.Lock(user.ID)
	defer unlock()

	loc := loadLocation(user.Timezone)
	ref := todayIn(loc)
	if d, ok := parseDateIn(req.AsOf, loc); ok {
		ref = d
	}
	summaries := a.sched.Rollover(user.ID, ref)
	writeJSON(w, 200, map[string]any{"asOf": dateStr(ref), "results": summaries})
}

func (a *API) handleICS(w http.ResponseWriter, r *http.Request, user *User) {
	plan, ok := a.ownedPlan(r.PathValue("id"), user)
	if !ok {
		writeAPIError(w, 404, codePlanNotFound)
		return
	}
	events := a.store.EventsForPlan(plan.ID)
	ics := a.sched.ICS(plan, events)

	// plan.Skill can be arbitrary user text, so it is reduced to a safe token
	// for the ASCII filename and carried verbatim only in the RFC 5987 form.
	pretty := "start-ai-" + plan.Skill + ".ics"
	safe := "start-ai-" + safeFilenameToken(plan.Skill) + ".ics"
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+safe+`"; filename*=UTF-8''`+rfc5987Encode(pretty))
	_, _ = w.Write([]byte(ics))
}

func (a *API) handleMeter(w http.ResponseWriter, r *http.Request, _ *User) {
	writeJSON(w, 200, a.gw.Snapshot())
}

// ---- small helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: response encode failed: %v", err)
	}
}

func readJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	// Unknown fields are ignored so older clients keep working; the body size is
	// already bounded by limitBody.
	return json.NewDecoder(r.Body).Decode(dst)
}
