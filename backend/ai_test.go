package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOpenAI stands in for the upstream API and records how it was called.
func fakeOpenAI(t *testing.T, handler func(n int64, w http.ResponseWriter)) (*httptest.Server, *int64) {
	t.Helper()
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		handler(n, w)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func okResponse(w http.ResponseWriter, content string) {
	resp := map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": content}}},
		"usage":   map[string]int{"prompt_tokens": 100, "completion_tokens": 50},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func testGateway(t *testing.T, base string) *Gateway {
	t.Helper()
	g := newGateway(Config{
		AILive: true, OpenAIKey: "sk-test", OpenAIBase: base,
		ModelFast: "gpt-4o-mini", ModelSmart: "gpt-4o",
		MaxConcurrency: 2, DailyCallsPerUser: 3, MonthlyUSDCap: 100,
		DataDir: t.TempDir(),
	})
	t.Cleanup(g.Close)
	return g
}

// The cache existed but every call site passed an empty key, so it never hit.
func TestGatewayCacheAvoidsRepeatCalls(t *testing.T) {
	srv, calls := fakeOpenAI(t, func(_ int64, w http.ResponseWriter) { okResponse(w, `{"ok":true}`) })
	g := testGateway(t, srv.URL)

	key := cacheKeyFor("understand", "en", "learn guitar")
	for i := 0; i < 5; i++ {
		out, err := g.Chat(context.Background(), "u1", "gpt-4o-mini", "sys", "learn guitar", true, key)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if out != `{"ok":true}` {
			t.Fatalf("call %d returned %q", i, out)
		}
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("upstream was called %d times for 5 identical requests, want 1", got)
	}
	if snap := g.Snapshot(); snap.CallsCached != 4 {
		t.Errorf("CallsCached = %d, want 4", snap.CallsCached)
	}
}

func TestCacheKeySeparatesDifferentInputs(t *testing.T) {
	a := cacheKeyFor("understand", "en", "learn guitar")
	b := cacheKeyFor("understand", "ru", "learn guitar")
	c := cacheKeyFor("understand", "en", "learn piano")
	d := cacheKeyFor("plan", "en", "learn guitar")
	keys := map[string]bool{a: true, b: true, c: true, d: true}
	if len(keys) != 4 {
		t.Errorf("cache keys collide: %v", keys)
	}
	if a != cacheKeyFor("understand", "en", "learn guitar") {
		t.Error("cache key is not stable for identical input")
	}
}

func TestGatewayCacheIsBounded(t *testing.T) {
	g := testGateway(t, "http://127.0.0.1:1")
	for i := 0; i < maxCacheEntries+200; i++ {
		g.cachePut("k"+itoa(i), "v")
	}
	g.cacheMu.RLock()
	size := len(g.cache)
	keys := len(g.cacheKeys)
	g.cacheMu.RUnlock()
	if size > maxCacheEntries {
		t.Errorf("cache holds %d entries, max is %d", size, maxCacheEntries)
	}
	if keys != size {
		t.Errorf("eviction list (%d) drifted from the cache (%d)", keys, size)
	}
	if _, ok := g.cacheGet("k0"); ok {
		t.Error("the oldest entry should have been evicted")
	}
	if _, ok := g.cacheGet("k" + itoa(maxCacheEntries+199)); !ok {
		t.Error("the newest entry should still be present")
	}
}

func TestGatewayEnforcesDailyQuota(t *testing.T) {
	srv, _ := fakeOpenAI(t, func(_ int64, w http.ResponseWriter) { okResponse(w, "hi") })
	g := testGateway(t, srv.URL) // DailyCallsPerUser: 3

	for i := 0; i < 3; i++ {
		if _, err := g.Chat(context.Background(), "u1", "gpt-4o-mini", "s", "u", false, ""); err != nil {
			t.Fatalf("call %d should have succeeded: %v", i, err)
		}
	}
	if _, err := g.Chat(context.Background(), "u1", "gpt-4o-mini", "s", "u", false, ""); err == nil {
		t.Error("the 4th call should have been refused by the daily quota")
	}
	// A different user is unaffected.
	if _, err := g.Chat(context.Background(), "u2", "gpt-4o-mini", "s", "u", false, ""); err != nil {
		t.Errorf("another user was starved by the first user's quota: %v", err)
	}
}

// A call that never produced an answer must not consume the user's quota.
func TestGatewayRefundsQuotaOnTotalFailure(t *testing.T) {
	srv, _ := fakeOpenAI(t, func(_ int64, w http.ResponseWriter) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	g := testGateway(t, srv.URL)

	if _, err := g.Chat(context.Background(), "u1", "gpt-4o-mini", "s", "u", false, ""); err == nil {
		t.Fatal("expected the call to fail")
	}
	g.mu.Lock()
	used := g.dailyUse["u1"]
	g.mu.Unlock()
	if used != 0 {
		t.Errorf("quota used = %d after a failed call, want 0 (it was refunded)", used)
	}
}

// A bad key fails identically on every model, so the fallback must be skipped.
func TestGatewayDoesNotRetryFallbackOnAuthFailure(t *testing.T) {
	srv, calls := fakeOpenAI(t, func(_ int64, w http.ResponseWriter) {
		http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
	})
	g := testGateway(t, srv.URL)

	if _, err := g.Chat(context.Background(), "u1", "gpt-4o", "s", "u", false, ""); err == nil {
		t.Fatal("expected an auth failure")
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("upstream was called %d times for a 401; want 1 (no retries, no fallback model)", got)
	}
}

func TestGatewayRetriesTransientFailures(t *testing.T) {
	srv, calls := fakeOpenAI(t, func(n int64, w http.ResponseWriter) {
		if n < 3 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		okResponse(w, "recovered")
	})
	g := testGateway(t, srv.URL)

	out, err := g.Chat(context.Background(), "u1", "gpt-4o-mini", "s", "u", false, "")
	if err != nil {
		t.Fatalf("should have recovered after retries: %v", err)
	}
	if out != "recovered" {
		t.Errorf("got %q", out)
	}
	if got := atomic.LoadInt64(calls); got != 3 {
		t.Errorf("upstream called %d times, want 3 (two 429s then success)", got)
	}
}

func TestGatewayFallsBackToTheFastModel(t *testing.T) {
	var sawSmart, sawFast atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body oaReq
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model == "gpt-4o" {
			sawSmart.Store(true)
			http.Error(w, "model unavailable", http.StatusBadRequest) // permanent for this model
			return
		}
		sawFast.Store(true)
		okResponse(w, "from fast model")
	}))
	defer srv.Close()

	g := testGateway(t, srv.URL)
	out, err := g.Chat(context.Background(), "u1", "gpt-4o", "s", "u", false, "")
	if err != nil {
		t.Fatalf("fallback should have succeeded: %v", err)
	}
	if out != "from fast model" {
		t.Errorf("got %q", out)
	}
	if !sawSmart.Load() || !sawFast.Load() {
		t.Errorf("expected both models to be tried (smart=%v fast=%v)", sawSmart.Load(), sawFast.Load())
	}
}

func TestGatewayHonoursTheMonthlySpendCap(t *testing.T) {
	srv, _ := fakeOpenAI(t, func(_ int64, w http.ResponseWriter) { okResponse(w, "hi") })
	g := newGateway(Config{
		AILive: true, OpenAIKey: "sk-test", OpenAIBase: srv.URL,
		ModelFast: "gpt-4o-mini", ModelSmart: "gpt-4o",
		MaxConcurrency: 2, DailyCallsPerUser: 0, MonthlyUSDCap: 0.00001,
		DataDir: t.TempDir(),
	})
	defer g.Close()

	if _, err := g.Chat(context.Background(), "u1", "gpt-4o", "s", "u", false, ""); err != nil {
		t.Fatalf("first call should go through: %v", err)
	}
	_, err := g.Chat(context.Background(), "u1", "gpt-4o", "s", "u", false, "")
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("expected the spend cap to stop the second call, got %v", err)
	}
}

// Quota and spend must outlive a restart; keeping them in memory silently reset
// the monthly cap and everyone's daily quota on every deploy.
func TestGatewayStateSurvivesRestart(t *testing.T) {
	srv, _ := fakeOpenAI(t, func(_ int64, w http.ResponseWriter) { okResponse(w, "hi") })
	dir := t.TempDir()
	cfg := Config{
		AILive: true, OpenAIKey: "sk-test", OpenAIBase: srv.URL,
		ModelFast: "gpt-4o-mini", ModelSmart: "gpt-4o",
		MaxConcurrency: 2, DailyCallsPerUser: 3, MonthlyUSDCap: 100, DataDir: dir,
	}

	g1 := newGateway(cfg)
	for i := 0; i < 3; i++ {
		if _, err := g1.Chat(context.Background(), "u1", "gpt-4o-mini", "s", "u", false, ""); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	before := g1.Snapshot()
	g1.Close()

	if _, err := os.Stat(filepath.Join(dir, "gateway.json")); err != nil {
		t.Fatalf("gateway state was not persisted: %v", err)
	}

	g2 := newGateway(cfg)
	defer g2.Close()
	after := g2.Snapshot()
	if after.CallsTotal != before.CallsTotal || after.TokensIn != before.TokensIn {
		t.Errorf("meter reset on restart: %+v -> %+v", before, after)
	}
	if _, err := g2.Chat(context.Background(), "u1", "gpt-4o-mini", "s", "u", false, ""); err == nil {
		t.Error("the daily quota reset on restart; the 4th call should still be refused")
	}
}

func TestGatewayDisabledWithoutAKey(t *testing.T) {
	g := newGateway(Config{DataDir: t.TempDir()})
	defer g.Close()
	if g.Enabled() {
		t.Error("gateway should be disabled with no API key")
	}
	if _, err := g.Chat(context.Background(), "u1", "m", "s", "u", false, ""); err == nil {
		t.Error("a disabled gateway should refuse calls rather than dial out")
	}
}

func TestGatewayRespectsContextCancellation(t *testing.T) {
	srv, _ := fakeOpenAI(t, func(_ int64, w http.ResponseWriter) { okResponse(w, "hi") })
	g := testGateway(t, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Chat(ctx, "u1", "gpt-4o-mini", "s", "u", false, ""); err == nil {
		t.Error("a cancelled context should abort the call")
	}
}

// ---- the AI_LIVE master switch ----

// A key alone is no longer enough: AI_LIVE has to agree, so a key can sit in
// .env without being spent.
func TestAILiveFalseKeepsMockModeDespiteAKey(t *testing.T) {
	srv, calls := fakeOpenAI(t, func(_ int64, w http.ResponseWriter) { okResponse(w, "hi") })
	g := newGateway(Config{
		AILive: false, OpenAIKey: "sk-test", OpenAIBase: srv.URL,
		ModelFast: "gpt-4o-mini", ModelSmart: "gpt-4o",
		MaxConcurrency: 2, DailyCallsPerUser: 3, MonthlyUSDCap: 100,
		DataDir: t.TempDir(),
	})
	defer g.Close()

	if g.Enabled() {
		t.Error("AI_LIVE=false must keep the gateway disabled even with a key set")
	}
	if _, err := g.Chat(context.Background(), "u1", "gpt-4o-mini", "s", "u", false, ""); err == nil {
		t.Error("a switched-off gateway should refuse the call rather than dial out")
	}
	if n := atomic.LoadInt64(calls); n != 0 {
		t.Errorf("the key was spent anyway: upstream called %d times, want 0", n)
	}
	if snap := g.Snapshot(); snap.Enabled {
		t.Error("GET /api/meter would report enabled=true while the switch is off")
	}
}

// The other half of the switch: AI_LIVE=true with no key is a misconfiguration,
// not a reason to dial out.
func TestAILiveTrueWithoutAKeyStaysMock(t *testing.T) {
	g := newGateway(Config{AILive: true, DataDir: t.TempDir()})
	defer g.Close()
	if g.Enabled() {
		t.Error("AI_LIVE=true without a key must not enable live mode")
	}
}

// Both halves present is the only combination that goes live.
func TestAILiveTrueWithAKeyGoesLive(t *testing.T) {
	srv, calls := fakeOpenAI(t, func(_ int64, w http.ResponseWriter) { okResponse(w, "hi") })
	g := testGateway(t, srv.URL)

	if !g.Enabled() {
		t.Fatal("AI_LIVE=true plus a key should be live")
	}
	if _, err := g.Chat(context.Background(), "u1", "gpt-4o-mini", "s", "u", false, ""); err != nil {
		t.Fatalf("live call failed: %v", err)
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("upstream called %d times, want 1", n)
	}
}

// AI_LIVE defaults to true so an existing deployment that only sets a key keeps
// working exactly as before.
func TestAILiveDefaultsToTrue(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	// Set-but-empty, so the real .env in this directory cannot supply a value
	// and the zero-config default is what is actually under test.
	t.Setenv("AI_LIVE", "")
	if cfg := loadConfig(); !cfg.AILive {
		t.Error("AI_LIVE should default to true when unset")
	}
	t.Setenv("AI_LIVE", "false")
	if cfg := loadConfig(); cfg.AILive {
		t.Error("AI_LIVE=false should be honoured")
	}
	t.Setenv("AI_LIVE", "true")
	if cfg := loadConfig(); !cfg.AILive {
		t.Error("AI_LIVE=true should be honoured")
	}
}

// The provider tells us when its window reopens. Ignoring that and retrying on
// our own 400ms/800ms/1200ms schedule meant all three attempts landed inside a
// window that still had eight seconds to run, and a call that would have
// succeeded was reported to the user as a failure.
func TestRetryAfterIsTakenFromTheProvider(t *testing.T) {
	groq429 := []byte(`{"error":{"message":"Rate limit reached for model ` +
		"`openai/gpt-oss-120b`" +
		` in organization ` + "`org_x`" + ` service tier ` + "`on_demand`" +
		` on tokens per minute (TPM): Limit 8000, Used 6272, Requested 2800. Please try again in 8.04s.","type":"tokens","code":"rate_limit_exceeded"}}`)

	if got := retryAfterFrom(http.Header{}, groq429); got < 8*time.Second || got > 9*time.Second {
		t.Errorf("parsed wait = %v, want ~8.04s from the message body", got)
	}

	cases := []struct {
		name string
		hdr  http.Header
		body []byte
		want time.Duration
	}{
		{"header wins over body", http.Header{"Retry-After": []string{"12"}}, groq429, 12 * time.Second},
		{"milliseconds", http.Header{}, []byte("please try again in 750ms"), 750 * time.Millisecond},
		{"minutes", http.Header{}, []byte("try again in 20m"), 20 * time.Minute},
		{"nothing to parse", http.Header{}, []byte("service unavailable"), 0},
		{"malformed header falls through", http.Header{"Retry-After": []string{"soon"}}, []byte("no hint"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryAfterFrom(tc.hdr, tc.body); got != tc.want {
				t.Errorf("retryAfterFrom = %v, want %v", got, tc.want)
			}
		})
	}
}

// A throttle that says "wait 20 minutes" must not hold a request open that long.
func TestRetryWaitIsBounded(t *testing.T) {
	if got := retryAfterFrom(http.Header{}, []byte("try again in 45m")); got <= maxRetryWait {
		t.Fatalf("parsed %v; the test needs a value above the cap to be meaningful", got)
	}
	// The cap is applied in Chat's retry loop; assert the constant is sane.
	if maxRetryWait < 5*time.Second || maxRetryWait > time.Minute {
		t.Errorf("maxRetryWait = %v, want a few seconds to a minute", maxRetryWait)
	}
}

// A throttled call must recover by waiting out the window the provider named,
// not fail after three sub-second retries that all land inside it.
func TestGatewayWaitsOutAThrottleAndSucceeds(t *testing.T) {
	srv, calls := fakeOpenAI(t, func(n int64, w http.ResponseWriter) {
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached on tokens per minute (TPM): Limit 8000, Used 6272, Requested 2800. Please try again in 0.4s.","code":"rate_limit_exceeded"}}`))
			return
		}
		okResponse(w, `{"ok":true}`)
	})
	g := testGateway(t, srv.URL)
	defer g.Close()

	start := time.Now()
	out, err := g.Chat(context.Background(), "u1", "gpt-4o", "sys", "user", true, "")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("a throttle with a 0.4s hint should have been waited out, got: %v", err)
	}
	if out != `{"ok":true}` {
		t.Errorf("content = %q, want the retried response", out)
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("retried after %v; the provider asked for 0.4s", elapsed)
	}
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Errorf("upstream called %d times, want 2 (throttled, then retried)", got)
	}
}

// The user's quota must survive a throttle that resolves: they got their answer.
func TestThrottleThatSucceedsStillChargesOnlyOneCall(t *testing.T) {
	srv, _ := fakeOpenAI(t, func(n int64, w http.ResponseWriter) {
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"try again in 0.2s"}}`))
			return
		}
		okResponse(w, "fine")
	})
	g := testGateway(t, srv.URL)
	defer g.Close()

	if _, err := g.Chat(context.Background(), "u1", "gpt-4o", "sys", "user", false, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	g.mu.Lock()
	used := g.dailyUse["u1"]
	g.mu.Unlock()
	if used != 1 {
		t.Errorf("dailyUse = %d, want 1: a retried call is still one delivered answer", used)
	}
}
