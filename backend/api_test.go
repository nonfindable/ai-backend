package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type testServer struct {
	*httptest.Server
	store *Store
	sched *Scheduler
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	return newTestServerCfg(t, nil)
}

// newTestServerCfg builds a server whose config the caller can adjust, so a test
// can run the API in live mode against a fake upstream.
func newTestServerCfg(t *testing.T, mutate func(*Config)) *testServer {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		Port: "0", DataDir: dir, FrontendDir: "", DefaultTimezone: "UTC",
		MaxConcurrency: 2, DailyCallsPerUser: 40,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	store := newStore(dir)
	gw := newGateway(cfg)
	sched := newScheduler(store)
	pipe := newPipeline(cfg, store, gw, sched)
	api := newAPI(cfg, store, gw, pipe, sched)

	srv := httptest.NewServer(api.routes())
	t.Cleanup(func() {
		srv.Close()
		gw.Close()
		store.Close()
	})
	return &testServer{Server: srv, store: store, sched: sched}
}

type client struct {
	t     *testing.T
	base  string
	token string
	user  string
}

func (ts *testServer) newClient(t *testing.T) *client {
	t.Helper()
	c := &client{t: t, base: ts.URL}
	var out map[string]any
	c.do("POST", "/api/session", map[string]any{"lang": "en", "timezone": "UTC"}, &out)
	tok, _ := out["token"].(string)
	if tok == "" {
		t.Fatal("POST /api/session did not return a token")
	}
	c.token, _ = out["token"].(string)
	c.user, _ = out["userId"].(string)
	return c
}

func (c *client) request(method, path string, body any) *http.Response {
	c.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp
}

func (c *client) do(method, path string, body, out any) int {
	c.t.Helper()
	resp := c.request(method, path, body)
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// buildPlan walks the mock intake to a finished plan and returns its ID.
func (c *client) buildPlan() (sessionID, planID string) {
	c.t.Helper()
	return c.buildPlanInZone("UTC")
}

// buildPlanInZone is buildPlan for a caller that cares which timezone the
// resulting plan is anchored to. POST /api/session updates the timezone of an
// already-authenticated user, so the zone has to be supplied here rather than
// only at first sign-in.
func (c *client) buildPlanInZone(zone string) (sessionID, planID string) {
	c.t.Helper()
	var s map[string]any
	c.do("POST", "/api/session", map[string]any{"lang": "en", "timezone": zone}, &s)
	sessionID, _ = s["sessionId"].(string)

	script := []string{"I want IELTS 7.0", "Academic", "band 5.5", "band 7.0",
		"2026-12-01", "10 hours a week on Mon Wed Fri", "free materials"}
	for _, msg := range script {
		var turn map[string]any
		c.do("POST", "/api/chat", map[string]any{"sessionId": sessionID, "message": msg}, &turn)
		if id, _ := turn["planId"].(string); id != "" {
			planID = id
		}
	}
	if planID == "" {
		c.t.Fatal("intake never produced a plan")
	}
	return sessionID, planID
}

// ---- authentication & authorization ----

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	ts := newTestServer(t)
	anon := &client{t: t, base: ts.URL} // no token

	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/chat", map[string]any{"sessionId": "x", "message": "hi"}},
		{"GET", "/api/plan/whatever", nil},
		{"GET", "/api/plan/whatever/ics", nil},
		{"POST", "/api/schedule", map[string]any{"planId": "x"}},
		{"POST", "/api/schedule/confirm", map[string]any{"planId": "x"}},
		{"GET", "/api/calendar", nil},
		{"POST", "/api/todo/complete", map[string]any{"planId": "x", "todoId": "y"}},
		{"POST", "/api/rollover", nil},
		{"GET", "/api/meter", nil},
	} {
		if got := anon.do(c.method, c.path, c.body, nil); got != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", c.method, c.path, got)
		}
	}
}

// The headline finding: one user could read and modify another user's plan.
func TestUsersCannotTouchOtherUsersPlans(t *testing.T) {
	ts := newTestServer(t)
	alice := ts.newClient(t)
	_, planID := alice.buildPlan()
	alice.do("POST", "/api/schedule", map[string]any{"planId": planID}, nil)

	mallory := ts.newClient(t)
	for _, c := range []struct {
		name, method, path string
		body               any
	}{
		{"read plan", "GET", "/api/plan/" + planID, nil},
		{"export ics", "GET", "/api/plan/" + planID + "/ics", nil},
		{"read calendar", "GET", "/api/calendar?planId=" + planID, nil},
		{"reschedule", "POST", "/api/schedule", map[string]any{"planId": planID}},
		{"confirm", "POST", "/api/schedule/confirm", map[string]any{"planId": planID}},
		{"complete todo", "POST", "/api/todo/complete", map[string]any{"planId": planID, "todoId": "t"}},
	} {
		if got := mallory.do(c.method, c.path, c.body, nil); got != http.StatusNotFound {
			t.Errorf("%s: %s %s = %d, want 404", c.name, c.method, c.path, got)
		}
	}

	// And the owner is unaffected.
	if got := alice.do("GET", "/api/plan/"+planID, nil, nil); got != http.StatusOK {
		t.Errorf("owner got %d reading their own plan", got)
	}
}

func TestSessionsCannotBeHijackedByID(t *testing.T) {
	ts := newTestServer(t)
	alice := ts.newClient(t)
	sessionID, _ := alice.buildPlan()

	mallory := ts.newClient(t)
	if got := mallory.do("POST", "/api/chat", map[string]any{"sessionId": sessionID, "message": "hi"}, nil); got != http.StatusNotFound {
		t.Errorf("another user continued someone else's conversation: status %d", got)
	}
}

func TestRolloverOnlyAffectsTheCallersPlans(t *testing.T) {
	ts := newTestServer(t)
	alice := ts.newClient(t)
	_, planID := alice.buildPlan()
	alice.do("POST", "/api/schedule", map[string]any{"planId": planID}, nil)

	mallory := ts.newClient(t)
	var out map[string]any
	mallory.do("POST", "/api/rollover", map[string]any{"asOf": "2027-01-01"}, &out)
	results, _ := out["results"].([]any)
	if len(results) != 0 {
		t.Errorf("rollover touched %d plans belonging to someone else", len(results))
	}
}

func TestInvalidTokenIsRejected(t *testing.T) {
	ts := newTestServer(t)
	bad := &client{t: t, base: ts.URL, token: "deadbeef"}
	if got := bad.do("GET", "/api/calendar", nil, nil); got != http.StatusUnauthorized {
		t.Errorf("a forged token was accepted: status %d", got)
	}
}

// ---- request hygiene ----

func TestOversizedBodyIsRejected(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	huge := strings.Repeat("A", maxRequestBytes+4096)
	status := c.do("POST", "/api/chat", map[string]any{"sessionId": "x", "message": huge}, nil)
	if status == http.StatusOK {
		t.Error("an oversized body was accepted")
	}
	if status != http.StatusBadRequest && status != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 400 or 413", status)
	}
}

func TestICSFilenameIsSanitized(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)

	var s map[string]any
	c.do("POST", "/api/session", map[string]any{"lang": "en", "timezone": "UTC"}, &s)
	sid, _ := s["sessionId"].(string)

	evil := `xx"; filename="owned.txt`
	planID := ""
	for _, msg := range []string{evil, "beginner", "goal", "2026-12-01", "5 hours", "free", "more"} {
		var turn map[string]any
		c.do("POST", "/api/chat", map[string]any{"sessionId": sid, "message": msg}, &turn)
		if id, _ := turn["planId"].(string); id != "" {
			planID = id
		}
	}
	if planID == "" {
		t.Fatal("no plan built")
	}

	resp := c.request("GET", "/api/plan/"+planID+"/ics", nil)
	defer resp.Body.Close()
	cd := resp.Header.Get("Content-Disposition")
	// Exactly one filename parameter: the injected `"; filename="` did not
	// close the quoted string and start a second one.
	if strings.Count(cd, `filename="`) != 1 {
		t.Errorf("user text broke out of the filename parameter: %q", cd)
	}
	const separators = "\";\r\n/\\"
	quoted := cd[strings.Index(cd, `filename="`)+len(`filename="`):]
	quoted = quoted[:strings.IndexByte(quoted, '"')]
	if strings.ContainsAny(quoted, separators) {
		t.Errorf("quoted filename still contains a separator: %q", quoted)
	}
	ext := cd[strings.Index(cd, "filename*=UTF-8''")+len("filename*=UTF-8''"):]
	if strings.ContainsAny(ext, separators+" =") {
		t.Errorf("RFC 5987 value is not fully percent-encoded: %q", ext)
	}
}

// ---- conversation integrity ----

// Concurrent turns on one session must not lose state or build two plans.
func TestConcurrentTurnsOnOneSessionStayConsistent(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)

	var s map[string]any
	c.do("POST", "/api/session", map[string]any{"lang": "en", "timezone": "UTC"}, &s)
	sid, _ := s["sessionId"].(string)
	for _, msg := range []string{"I want to learn guitar", "Acoustic", "beginner", "play 5 songs"} {
		c.do("POST", "/api/chat", map[string]any{"sessionId": sid, "message": msg}, nil)
	}

	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.do("POST", "/api/chat", map[string]any{"sessionId": sid, "message": "2026-12-01"}, nil)
		}()
	}
	wg.Wait()

	sess, ok := ts.store.GetSession(sid)
	if !ok {
		t.Fatal("session vanished")
	}
	// 4 sequential + n concurrent user turns, each with an assistant reply.
	wantUser := 4 + n
	gotUser := 0
	for _, m := range sess.Messages {
		if m.Role == "user" {
			gotUser++
		}
	}
	if gotUser != wantUser {
		t.Errorf("stored %d user messages, want %d — turns raced and lost appends", gotUser, wantUser)
	}

	plans := ts.store.PlansByUser(c.user)
	if len(plans) > 1 {
		t.Errorf("one conversation produced %d plans", len(plans))
	}
}

// ---- completion semantics ----

func TestCompletingOneSessionDoesNotCancelTheSeries(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	_, planID := c.buildPlan()

	var sched map[string]any
	c.do("POST", "/api/schedule", map[string]any{"planId": planID}, &sched)
	before := int(sched["count"].(float64))

	var plan Plan
	c.do("GET", "/api/plan/"+planID, nil, &plan)
	var recurring *Todo
	for i := range plan.Todos {
		if plan.Todos[i].Frequency != "once" {
			recurring = &plan.Todos[i]
			break
		}
	}
	if recurring == nil {
		t.Fatal("no recurring todo in the plan")
	}

	var done map[string]any
	c.do("POST", "/api/todo/complete", map[string]any{"planId": planID, "todoId": recurring.ID}, &done)
	if done["status"] == "done" {
		t.Errorf("one completed session marked the whole %s todo done", recurring.Frequency)
	}
	if remaining, _ := done["remaining"].(float64); remaining <= 0 {
		t.Errorf("remaining = %v after completing one of several sessions", done["remaining"])
	}

	var resched map[string]any
	c.do("POST", "/api/schedule", map[string]any{"planId": planID}, &resched)
	after := int(resched["count"].(float64))
	if after < before-2 {
		t.Errorf("rescheduling after one completion dropped the series: %d -> %d events", before, after)
	}
}

func TestConfirmOnlyPromotesProposedEvents(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	_, planID := c.buildPlan()
	c.do("POST", "/api/schedule", map[string]any{"planId": planID}, nil)

	var first map[string]any
	c.do("POST", "/api/schedule/confirm", map[string]any{"planId": planID}, &first)
	n1, _ := first["confirmed"].(float64)
	if n1 == 0 {
		t.Fatal("nothing was confirmed")
	}
	var second map[string]any
	c.do("POST", "/api/schedule/confirm", map[string]any{"planId": planID}, &second)
	if n2, _ := second["confirmed"].(float64); n2 != 0 {
		t.Errorf("second confirm re-confirmed %v events, want 0", n2)
	}
}

// ---- plan content ----

func TestPlanCarriesDeadlineAndSchedulingMetadata(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	_, planID := c.buildPlan()

	var plan Plan
	c.do("GET", "/api/plan/"+planID, nil, &plan)
	if plan.Deadline != "2026-12-01" {
		t.Errorf("plan.Deadline = %q, want the deadline given during intake", plan.Deadline)
	}
	if plan.HoursPerWeek != 10 {
		t.Errorf("plan.HoursPerWeek = %d, want 10", plan.HoursPerWeek)
	}
	wantDays := []string{"Mon", "Wed", "Fri"}
	if strings.Join(plan.Days, ",") != strings.Join(wantDays, ",") {
		t.Errorf("plan.Days = %v, want %v", plan.Days, wantDays)
	}
	if plan.Timezone != "UTC" {
		t.Errorf("plan.Timezone = %q, want UTC", plan.Timezone)
	}
	for _, ph := range plan.Phases {
		if ph.WeekStart < 1 || ph.WeekEnd < ph.WeekStart || ph.WeekEnd > plan.WeeksTotal {
			t.Errorf("phase %s has an invalid window [%d,%d] for a %d-week plan", ph.Key, ph.WeekStart, ph.WeekEnd, plan.WeeksTotal)
		}
	}
}

func TestProfileRemembersAvailabilityAfterFirstPlan(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	c.buildPlan()

	u, ok := ts.store.GetUser(c.user)
	if !ok {
		t.Fatal("user missing")
	}
	if u.HoursPerWeek != 10 {
		t.Errorf("profile HoursPerWeek = %d, want 10 carried over from intake", u.HoursPerWeek)
	}
	if len(u.Days) == 0 {
		t.Error("profile did not remember the available days")
	}
}

func TestHealthzNeedsNoAuth(t *testing.T) {
	ts := newTestServer(t)
	anon := &client{t: t, base: ts.URL}
	if got := anon.do("GET", "/healthz", nil, nil); got != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", got)
	}
}

// With a frontend directory present the API is mounted behind a second mux.
// That branch must still match the API routes, including path wildcards.
func TestAPIStillRoutesWhenServingAFrontend(t *testing.T) {
	dir := t.TempDir()
	frontend := t.TempDir()
	if err := os.WriteFile(filepath.Join(frontend, "index.html"), []byte("<h1>start.ai</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		Port: "0", DataDir: dir, FrontendDir: frontend, DefaultTimezone: "UTC",
		MaxConcurrency: 2, DailyCallsPerUser: 40,
	}
	store := newStore(dir)
	gw := newGateway(cfg)
	sched := newScheduler(store)
	api := newAPI(cfg, store, gw, newPipeline(cfg, store, gw, sched), sched)
	srv := httptest.NewServer(api.routes())
	defer func() {
		srv.Close()
		gw.Close()
		store.Close()
	}()

	ts := &testServer{Server: srv, store: store}
	c := ts.newClient(t)

	// Static file is served at the root.
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "start.ai") {
		t.Errorf("frontend not served at /: %q", string(body))
	}

	// And the API — including the {id} wildcard — still works behind it.
	_, planID := c.buildPlan()
	var plan Plan
	if got := c.do("GET", "/api/plan/"+planID, nil, &plan); got != http.StatusOK {
		t.Fatalf("GET /api/plan/{id} = %d behind the frontend mux", got)
	}
	if plan.ID != planID {
		t.Errorf("path wildcard lost: got plan %q, want %q", plan.ID, planID)
	}
	if got := c.do("GET", "/healthz", nil, nil); got != http.StatusOK {
		t.Errorf("/healthz = %d behind the frontend mux", got)
	}
	// Auth is still enforced on that path.
	anon := &client{t: t, base: srv.URL}
	if got := anon.do("GET", "/api/plan/"+planID, nil, nil); got != http.StatusUnauthorized {
		t.Errorf("auth bypassed behind the frontend mux: %d", got)
	}
}
