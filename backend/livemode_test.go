package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// P2: live mode must never silently become mock mode.
// P3: upstream text must never reach a client.

// liveServer wires the API to a fake upstream, in live mode.
func liveServer(t *testing.T, handler http.HandlerFunc) (*testServer, *int64) {
	t.Helper()
	var calls int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		handler(w, r)
	}))
	t.Cleanup(up.Close)
	ts := newTestServerCfg(t, func(c *Config) {
		c.AILive = true
		c.OpenAIKey = "sk-test"
		c.OpenAIBase = up.URL
		c.ModelFast = "gpt-4o-mini"
		c.ModelSmart = "gpt-4o"
		c.MonthlyUSDCap = 100
	})
	return ts, &calls
}

// The exact body OpenAI returned in production when the account ran dry. Every
// token in it is something a client must never see.
const billingLeak = `{"error":{"message":"You have no credits remaining. Add credits to continue using the API at https://platform.openai.com/settings/organization/billing/.","type":"insufficient_quota","code":"credit_balance_exhausted"}}`

var leakMarkers = []string{
	"credits", "billing", "platform.openai.com", "insufficient_quota",
	"credit_balance_exhausted", "openai", "sk-test", "OPENAI_API_KEY",
}

func assertNoLeak(t *testing.T, body string) {
	t.Helper()
	low := strings.ToLower(body)
	for _, m := range leakMarkers {
		if strings.Contains(low, strings.ToLower(m)) {
			t.Errorf("client response leaked upstream detail %q: %s", m, body)
		}
	}
}

func chatRaw(t *testing.T, c *client, sessionID, msg string) (int, string) {
	t.Helper()
	resp := c.request("POST", "/api/chat", map[string]any{"sessionId": sessionID, "message": msg})
	defer resp.Body.Close()
	buf := make([]byte, 64<<10)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}

func newSession(t *testing.T, c *client) string {
	t.Helper()
	var s map[string]any
	c.do("POST", "/api/session", map[string]any{"lang": "en", "timezone": "UTC"}, &s)
	id, _ := s["sessionId"].(string)
	if id == "" {
		t.Fatal("no sessionId")
	}
	return id
}

// A dead upstream must surface as an error, not as a canned plan.
func TestLiveUpstreamFailureDoesNotFallBackToMock(t *testing.T) {
	ts, calls := liveServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(billingLeak))
	})
	c := ts.newClient(t)
	sess := newSession(t, c)

	status, body := chatRaw(t, c, sess, "I want IELTS 7.0 by October")
	if status == http.StatusOK {
		t.Fatalf("a failed live call returned 200 with a body the user will read as real: %s", body)
	}
	if atomic.LoadInt64(calls) == 0 {
		t.Fatal("upstream was never called; the test did not exercise live mode")
	}

	// The mock brain recognises "IELTS" and produces a primer. None of it may
	// appear when the live path failed.
	for _, mockMarker := range []string{"IELTS has four sections", "Listening", "Academic and General"} {
		if strings.Contains(body, mockMarker) {
			t.Errorf("live failure silently served mock content (%q): %s", mockMarker, body)
		}
	}
	assertNoLeak(t, body)

	var env apiErrorEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("error body is not the documented envelope: %v (%s)", err, body)
	}
	if env.Error.Code != codeAIRateLimited && env.Error.Code != codeAIUnavailable {
		t.Errorf("code = %q, want AI_RATE_LIMITED or AI_UNAVAILABLE", env.Error.Code)
	}
	if env.Error.Message == "" {
		t.Error("error carried no client-safe message")
	}
}

// Malformed JSON from a live model is a live failure too.
func TestLiveMalformedResponseDoesNotFallBackToMock(t *testing.T) {
	ts, _ := liveServer(t, func(w http.ResponseWriter, r *http.Request) {
		okResponse(w, "this is not JSON at all")
	})
	c := ts.newClient(t)
	sess := newSession(t, c)

	status, body := chatRaw(t, c, sess, "I want to learn guitar")
	if status == http.StatusOK {
		t.Fatalf("malformed live JSON was accepted and answered: %s", body)
	}
	if strings.Contains(body, "acoustic") || strings.Contains(body, "Acoustic") {
		t.Errorf("mock guitar content leaked into a live failure: %s", body)
	}
	assertNoLeak(t, body)
}

// A live failure at the intake stage must not hand the conversation to the mock
// question set. Call 1 (understand) succeeds; call 2 (intake) fails.
func TestLiveIntakeFailureDoesNotFallBackToMock(t *testing.T) {
	var n int64
	ts, _ := liveServer(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&n, 1) == 1 {
			okResponse(w, `{"inScope":true,"skill":"Guitar","needsDisambiguation":false,"overview":"A live overview."}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(billingLeak))
	})
	c := ts.newClient(t)
	sess := newSession(t, c)

	status, body := chatRaw(t, c, sess, "I want to learn guitar")
	if status == http.StatusOK {
		t.Fatalf("intake stage failed live but the turn succeeded: %s", body)
	}
	// These are the deterministic mock interview questions.
	for _, q := range []string{"What's your current level", "where are you starting from", "Quick fork"} {
		if strings.Contains(body, q) {
			t.Errorf("mock intake question served after a live failure (%q): %s", q, body)
		}
	}
	assertNoLeak(t, body)
}

// A live plan-stage failure must not produce a plan at all — a mock plan here
// would be indistinguishable from a real one to the user.
func TestLivePlanFailureProducesNoPlan(t *testing.T) {
	var n int64
	ts, _ := liveServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt64(&n, 1) {
		case 1:
			okResponse(w, `{"inScope":true,"skill":"Guitar","needsDisambiguation":false,"overview":"ov"}`)
		case 2, 3, 4, 5, 6:
			okResponse(w, `{"answers":{"currentLevel":"beginner","target":"play songs","hoursPerWeek":6,"days":["Mon"]},"nextQuestion":"","done":true}`)
		default:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(billingLeak))
		}
	})
	c := ts.newClient(t)
	sess := newSession(t, c)

	_, _ = chatRaw(t, c, sess, "I want to learn guitar")
	status, body := chatRaw(t, c, sess, "beginner, 6 hours a week on Mon")
	if status == http.StatusOK {
		var turn Turn
		_ = json.Unmarshal([]byte(body), &turn)
		if turn.PlanID != "" {
			t.Fatalf("a plan was produced despite the live plan stage failing: %s", turn.PlanID)
		}
	}
	assertNoLeak(t, body)

	if plans := ts.store.PlansByUser(c.user); len(plans) != 0 {
		t.Errorf("%d plan(s) persisted after a live plan-stage failure", len(plans))
	}
}

// Mock mode must stay fully functional — it is the demo path.
func TestMockModeStillProducesAPlanEndToEnd(t *testing.T) {
	ts := newTestServer(t) // no key -> mock
	c := ts.newClient(t)
	_, planID := c.buildPlan()
	if planID == "" {
		t.Fatal("mock mode failed to produce a plan")
	}
	if plans := ts.store.PlansByUser(c.user); len(plans) != 1 {
		t.Errorf("expected exactly one plan in mock mode, got %d", len(plans))
	}
}

// Every error the API emits must use the documented envelope and a known code.
func TestAllAPIErrorsUseTheStableEnvelope(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	anon := &client{t: t, base: ts.URL}

	cases := []struct {
		name, method, path string
		body               any
		c                  *client
		wantCode           string
	}{
		{"unauthenticated", "GET", "/api/calendar", nil, anon, codeUnauthorized},
		{"missing plan", "GET", "/api/plan/plan_nope", nil, c, codePlanNotFound},
		{"missing session", "POST", "/api/chat", map[string]any{"sessionId": "sess_nope", "message": "hi"}, c, codeSessionNotFound},
		{"empty message", "POST", "/api/chat", map[string]any{"sessionId": "x", "message": ""}, c, codeInvalidRequest},
		{"foreign plan schedule", "POST", "/api/schedule", map[string]any{"planId": "plan_nope"}, c, codePlanNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := tc.c.request(tc.method, tc.path, tc.body)
			defer resp.Body.Close()
			var env apiErrorEnvelope
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("not the documented envelope: %v", err)
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.wantCode)
			}
			if env.Error.Message != safeMessage(tc.wantCode) {
				t.Errorf("message = %q, want the canned message for %s", env.Error.Message, tc.wantCode)
			}
		})
	}
}

// Ownership probing must stay indistinguishable from a missing object.
func TestForeignPlanIsIndistinguishableFromMissing(t *testing.T) {
	ts := newTestServer(t)
	owner := ts.newClient(t)
	other := ts.newClient(t)
	_, planID := owner.buildPlan()

	real := other.request("GET", "/api/plan/"+planID, nil)
	defer real.Body.Close()
	fake := other.request("GET", "/api/plan/plan_doesnotexist", nil)
	defer fake.Body.Close()

	if real.StatusCode != fake.StatusCode {
		t.Errorf("existing-but-foreign plan returned %d, nonexistent returned %d — IDs are probeable",
			real.StatusCode, fake.StatusCode)
	}
	var a, b apiErrorEnvelope
	_ = json.NewDecoder(real.Body).Decode(&a)
	_ = json.NewDecoder(fake.Body).Decode(&b)
	if a.Error.Code != b.Error.Code {
		t.Errorf("codes differ: %q vs %q", a.Error.Code, b.Error.Code)
	}
}

// The quota refund on total failure must survive the error-handling rework.
func TestQuotaIsRefundedWhenLiveCallsFail(t *testing.T) {
	ts, _ := liveServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(billingLeak))
	})
	c := ts.newClient(t)
	sess := newSession(t, c)
	for i := 0; i < 3; i++ {
		_, _ = chatRaw(t, c, sess, "I want IELTS 7.0")
	}
	var meter MeterSnapshot
	c.do("GET", "/api/meter", nil, &meter)
	if meter.CallsTotal != 0 {
		t.Errorf("callsTotal = %d, want 0 — failed calls were metered as successes", meter.CallsTotal)
	}
	if meter.CostUSD != 0 {
		t.Errorf("costUsd = %v, want 0", meter.CostUSD)
	}
}
