package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Gateway is the single point every model call flows through — no client ever
// talks to OpenAI directly. It absorbs the unpredictability of one shared API
// account: per-user quotas, a concurrency cap, response caching, retry with
// fallback, and cost metering. (See §11 "One API, many users" in the plan.)
type Gateway struct {
	cfg  Config
	http *http.Client

	sem chan struct{} // concurrency cap

	mu       sync.Mutex
	dayKey   string
	monthKey string
	dailyUse map[string]int // userID -> calls today

	cacheMu   sync.RWMutex
	cache     map[string]string
	cacheKeys []string // insertion order, for bounded FIFO eviction

	meterMu    sync.Mutex
	tokensIn   int
	tokensOut  int
	costUSD    float64
	callsTotal int
	callsCache int

	stateDir string
	dirty    chan struct{}
	done     chan struct{}
	closeOne sync.Once
	wg       sync.WaitGroup
}

// maxCacheEntries bounds the response cache so a long-running process cannot
// grow it without limit.
const maxCacheEntries = 512

func newGateway(cfg Config) *Gateway {
	conc := cfg.MaxConcurrency
	if conc < 1 {
		conc = 1
	}
	g := &Gateway{
		cfg:      cfg,
		http:     &http.Client{Timeout: 60 * time.Second},
		sem:      make(chan struct{}, conc),
		dailyUse: map[string]int{},
		cache:    map[string]string{},
		stateDir: cfg.DataDir,
		dirty:    make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	g.loadState()
	g.wg.Add(1)
	go g.stateLoop()
	return g
}

// Enabled reports whether live generation is switched on: AI_LIVE must be true
// AND a key must be present. When false the pipeline runs in mock mode and
// never calls this gateway's network path.
func (g *Gateway) Enabled() bool { return g.cfg.AILive && g.cfg.OpenAIKey != "" }

// cacheKeyFor builds a stable cache key for a pipeline stage. Identical inputs
// produce identical model output, so two users asking the same thing share one
// paid call — which is the entire point of the cache that previously sat unused
// because every call site passed an empty key.
func cacheKeyFor(stage, lang, payload string) string {
	sum := sha256.Sum256([]byte(stage + "\x00" + normLang(lang) + "\x00" + payload))
	return stage + ":" + hex.EncodeToString(sum[:16])
}

// rough per-1K-token USD rates, used only for the demo cost meter. Both the
// OpenAI and Groq model families are listed because the gateway is provider-
// agnostic (any OpenAI-compatible base URL works); an unlisted model falls to a
// conservative default so the monthly cap still trips rather than never firing.
func modelRates(model string) (in, out float64) {
	switch model {
	// OpenAI
	case "gpt-4o":
		return 0.0025, 0.01
	case "gpt-4o-mini":
		return 0.00015, 0.0006
	// Groq (approximate public list prices; free tier bills at $0).
	case "llama-3.3-70b-versatile":
		return 0.00059, 0.00079
	case "llama-3.1-8b-instant":
		return 0.00005, 0.00008
	case "openai/gpt-oss-120b":
		return 0.00015, 0.0006
	default: // unknown model: assume the cheap tier
		return 0.00015, 0.0006
	}
}

type oaMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaReq struct {
	Model       string  `json:"model"`
	Messages    []oaMsg `json:"messages"`
	Temperature float64 `json:"temperature"`
	// MaxCompletionTokens is the modern spelling accepted by both the OpenAI
	// API and Groq; max_tokens is the deprecated alias. Omitted when 0 so a
	// provider that knows neither still works.
	MaxCompletionTokens int           `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string        `json:"reasoning_effort,omitempty"`
	ResponseFormat      *oaRespFormat `json:"response_format,omitempty"`
}

type oaRespFormat struct {
	Type string `json:"type"`
}

type oaResp struct {
	Choices []struct {
		Message      oaMsg  `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// callClass says how a failed attempt should be handled.
type callClass int

const (
	classOK          callClass = iota
	classRetry                 // transient: retry the same model
	classRateLimited           // throttled or out of capacity: retry, then report as such
	classPermanent             // this model won't work: try the fallback model
	classFatal                 // auth/config problem: no model will work
	classTruncated             // the answer was cut off by the token cap
)

// Chat runs one completion through all the gateway protections and returns the
// assistant text. cacheKey (when non-empty) enables caching for repeatable calls.
func (g *Gateway) Chat(ctx context.Context, userID, model, system, user string, jsonMode bool, cacheKey string) (string, error) {
	if !g.Enabled() {
		return "", fmt.Errorf("%w: set AI_LIVE=true and OPENAI_API_KEY", errGatewayDisabled)
	}

	// 1) cache — the most-repeated calls (skill overviews, question templates)
	//    become zero-cost, zero-latency hits.
	if cacheKey != "" {
		if v, ok := g.cacheGet(cacheKey); ok {
			g.meterMu.Lock()
			g.callsCache++
			g.meterMu.Unlock()
			g.markDirty()
			return v, nil
		}
	}

	// 2) hard monthly spend cap — checked before queueing so it fails fast.
	g.meterMu.Lock()
	over := g.cfg.MonthlyUSDCap > 0 && g.costUSD >= g.cfg.MonthlyUSDCap
	g.meterMu.Unlock()
	if over {
		return "", fmt.Errorf("%w (cap $%.2f)", errSpendCapReached, g.cfg.MonthlyUSDCap)
	}

	// 3) concurrency cap (backpressure).
	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// 4) per-user daily quota — one heavy user can never starve the rest.
	//    Claimed after the semaphore so a request that gives up while queued
	//    was never charged for a call it did not make.
	if err := g.claimQuota(userID); err != nil {
		return "", err
	}

	// 5) call with retry + fallback to the fast model on repeated failure.
	models := []string{model}
	if model != g.cfg.ModelFast {
		models = append(models, g.cfg.ModelFast)
	}
	var lastErr error
	lastClass := classPermanent
	for _, m := range models {
		fatal := false
		for attempt := 0; attempt < 3; attempt++ {
			out, class, wait, err := g.callOnce(ctx, m, system, user, jsonMode)
			lastClass = class
			if err == nil && class == classOK {
				if cacheKey != "" {
					g.cachePut(cacheKey, out)
				}
				return out, nil
			}
			lastErr = err
			if class == classFatal || class == classTruncated {
				// A bad key or a disabled account will fail identically on every
				// model; stop instead of burning a second round trip. A response
				// that hit the token cap will hit it again on the retry, and each
				// attempt is charged for a full cap's worth of output — so it
				// stops here too, with an error that names the setting to change.
				fatal = true
				break
			}
			if class != classRetry && class != classRateLimited {
				break
			}
			// Back off by our own schedule, unless the provider named one —
			// it knows when its window reopens and we do not.
			delay := time.Duration(400*(attempt+1)) * time.Millisecond
			if wait > delay {
				delay = wait
				log.Printf("gateway: throttled by provider, waiting %s before retry %d (model=%s)", delay.Round(100*time.Millisecond), attempt+2, m)
			}
			if delay > maxRetryWait {
				delay = maxRetryWait
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				g.refundQuota(userID)
				return "", ctx.Err()
			}
		}
		if fatal {
			break
		}
	}
	// Nothing was delivered, so do not hold the user's quota against them.
	g.refundQuota(userID)

	// The provider's own words stay in the log only. The returned error carries
	// a sentinel so the API can pick a stable code, plus the detail for the
	// server-side log line — callers must never render it.
	sentinel := errAIUnavailable
	if lastClass == classRateLimited {
		sentinel = errAIRateLimited
	}
	log.Printf("gateway: all attempts failed (model=%s): %v", model, lastErr)
	return "", fmt.Errorf("%w: %v", sentinel, lastErr)
}

func (g *Gateway) cacheGet(key string) (string, bool) {
	g.cacheMu.RLock()
	defer g.cacheMu.RUnlock()
	v, ok := g.cache[key]
	return v, ok
}

func (g *Gateway) cachePut(key, val string) {
	g.cacheMu.Lock()
	defer g.cacheMu.Unlock()
	if _, exists := g.cache[key]; !exists {
		if len(g.cacheKeys) >= maxCacheEntries {
			oldest := g.cacheKeys[0]
			g.cacheKeys = g.cacheKeys[1:]
			delete(g.cache, oldest)
		}
		g.cacheKeys = append(g.cacheKeys, key)
	}
	g.cache[key] = val
}

// rollPeriodsLocked resets the per-day quota and the per-month spend when the
// calendar moves on. Caller holds g.mu.
func (g *Gateway) rollPeriodsLocked(now time.Time) {
	if day := now.Format(dateLayout); g.dayKey != day {
		g.dayKey = day
		g.dailyUse = map[string]int{}
	}
	if month := now.Format("2006-01"); g.monthKey != month {
		g.monthKey = month
		g.meterMu.Lock()
		g.costUSD = 0
		g.meterMu.Unlock()
	}
}

func (g *Gateway) claimQuota(userID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollPeriodsLocked(time.Now())
	if g.cfg.DailyCallsPerUser <= 0 {
		return nil
	}
	if g.dailyUse[userID] >= g.cfg.DailyCallsPerUser {
		return fmt.Errorf("%w (%d calls)", errDailyQuota, g.cfg.DailyCallsPerUser)
	}
	g.dailyUse[userID]++
	g.markDirty()
	return nil
}

func (g *Gateway) refundQuota(userID string) {
	if g.cfg.DailyCallsPerUser <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dailyUse[userID] > 0 {
		g.dailyUse[userID]--
	}
	g.markDirty()
}

// maxRetryWait bounds how long one attempt will sit waiting for a throttle to
// lift. Past this the request is better off failing and being retried by the
// caller than holding a connection and a concurrency slot indefinitely.
const maxRetryWait = 30 * time.Second

// reRetryAfter reads the wait out of a provider message such as
// "Please try again in 8.04s" when no Retry-After header is present.
var reRetryAfter = regexp.MustCompile(`try again in ([0-9]+(?:\.[0-9]+)?)(ms|s|m)`)

// retryAfterFrom extracts how long the provider says to wait. Guessing shorter
// than it asks for wastes the attempt: on a per-minute token limit the window
// reopens on the provider's schedule, not ours, and three 400ms backoffs all
// land inside a window that had eight seconds left to run.
func retryAfterFrom(h http.Header, body []byte) time.Duration {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
			return time.Duration(secs * float64(time.Second))
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	if m := reRetryAfter.FindSubmatch(body); m != nil {
		n, err := strconv.ParseFloat(string(m[1]), 64)
		if err != nil || n <= 0 {
			return 0
		}
		switch string(m[2]) {
		case "ms":
			return time.Duration(n * float64(time.Millisecond))
		case "m":
			return time.Duration(n * float64(time.Minute))
		default:
			return time.Duration(n * float64(time.Second))
		}
	}
	return 0
}

func (g *Gateway) callOnce(ctx context.Context, model, system, user string, jsonMode bool) (string, callClass, time.Duration, error) {
	reqBody := oaReq{
		Model:               model,
		Temperature:         0.4,
		MaxCompletionTokens: g.cfg.MaxCompletionTokens,
		ReasoningEffort:     g.cfg.ReasoningEffort,
		Messages: []oaMsg{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}
	if jsonMode {
		reqBody.ResponseFormat = &oaRespFormat{Type: "json_object"}
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return "", classPermanent, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.OpenAIBase+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", classPermanent, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.cfg.OpenAIKey)

	resp, err := g.http.Do(req)
	if err != nil {
		return "", classRetry, 0, err // network error — retryable
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return "", classFatal, 0, fmt.Errorf("openai status %d: %s", resp.StatusCode, string(body))
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", classRateLimited, retryAfterFrom(resp.Header, body), fmt.Errorf("openai status %d: %s", resp.StatusCode, string(body))
	case resp.StatusCode >= 500:
		return "", classRetry, retryAfterFrom(resp.Header, body), fmt.Errorf("openai status %d: %s", resp.StatusCode, string(body))
	case resp.StatusCode == http.StatusBadRequest && bytes.Contains(body, []byte("json_validate_failed")):
		// The provider validated the JSON itself and rejected it. With an empty
		// failed_generation this is the token cap cutting the object in half,
		// not a prompt the model cannot follow — report it as truncation so the
		// operator is pointed at AI_MAX_COMPLETION_TOKENS rather than at the prompt.
		return "", classTruncated, 0, fmt.Errorf("openai status %d (json_validate_failed): %s", resp.StatusCode, string(body))
	case resp.StatusCode != http.StatusOK:
		return "", classPermanent, 0, fmt.Errorf("openai status %d: %s", resp.StatusCode, string(body))
	}

	var parsed oaResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", classPermanent, 0, err
	}
	if parsed.Error != nil {
		return "", classPermanent, 0, errors.New(parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", classRetry, 0, errors.New("no choices returned")
	}

	// Tokens were spent whatever the outcome, so meter before any early return.
	g.meter(model, parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens)

	// A cut-off answer is a complete HTTP 200 carrying half a JSON object. Left
	// unchecked it surfaces downstream as "the model returned unparsable JSON",
	// which sends whoever reads the log looking for a prompt bug that is not there.
	if parsed.Choices[0].FinishReason == "length" {
		return "", classTruncated, 0, fmt.Errorf("response truncated after %d completion tokens (cap %d); raise AI_MAX_COMPLETION_TOKENS or set AI_REASONING_EFFORT=low",
			parsed.Usage.CompletionTokens, g.cfg.MaxCompletionTokens)
	}
	return parsed.Choices[0].Message.Content, classOK, 0, nil
}

func (g *Gateway) meter(model string, in, out int) {
	inRate, outRate := modelRates(model)
	g.meterMu.Lock()
	g.tokensIn += in
	g.tokensOut += out
	g.costUSD += float64(in)/1000*inRate + float64(out)/1000*outRate
	g.callsTotal++
	g.meterMu.Unlock()
	g.markDirty()
}

// MeterSnapshot is the read model behind GET /api/meter.
type MeterSnapshot struct {
	Enabled     bool    `json:"enabled"`
	CallsTotal  int     `json:"callsTotal"`
	CallsCached int     `json:"callsCached"`
	TokensIn    int     `json:"tokensIn"`
	TokensOut   int     `json:"tokensOut"`
	CostUSD     float64 `json:"costUsd"`
	MonthlyCap  float64 `json:"monthlyCapUsd"`
}

func (g *Gateway) Snapshot() MeterSnapshot {
	g.meterMu.Lock()
	defer g.meterMu.Unlock()
	return MeterSnapshot{
		Enabled:     g.Enabled(),
		CallsTotal:  g.callsTotal,
		CallsCached: g.callsCache,
		TokensIn:    g.tokensIn,
		TokensOut:   g.tokensOut,
		CostUSD:     g.costUSD,
		MonthlyCap:  g.cfg.MonthlyUSDCap,
	}
}

// ---- persistence ----
//
// Quota and spend live across restarts. Keeping them purely in memory meant a
// redeploy silently reset both the monthly cap and every user's daily quota.

type gatewayState struct {
	DayKey     string         `json:"dayKey"`
	MonthKey   string         `json:"monthKey"`
	DailyUse   map[string]int `json:"dailyUse"`
	TokensIn   int            `json:"tokensIn"`
	TokensOut  int            `json:"tokensOut"`
	CostUSD    float64        `json:"costUsd"`
	CallsTotal int            `json:"callsTotal"`
	CallsCache int            `json:"callsCached"`
}

func (g *Gateway) statePath() string { return filepath.Join(g.stateDir, "gateway.json") }

func (g *Gateway) loadState() {
	data, err := os.ReadFile(g.statePath())
	if err != nil {
		return
	}
	var st gatewayState
	if json.Unmarshal(data, &st) != nil {
		log.Printf("gateway: ignoring unreadable %s", g.statePath())
		return
	}
	g.dayKey, g.monthKey = st.DayKey, st.MonthKey
	if st.DailyUse != nil {
		g.dailyUse = st.DailyUse
	}
	g.tokensIn, g.tokensOut = st.TokensIn, st.TokensOut
	g.costUSD, g.callsTotal, g.callsCache = st.CostUSD, st.CallsTotal, st.CallsCache
	// rollPeriodsLocked documents that the caller holds g.mu. It is true that
	// nothing else runs yet during construction, but honouring the contract
	// costs nothing and stops the next caller inheriting a latent race.
	g.mu.Lock()
	g.rollPeriodsLocked(time.Now())
	g.mu.Unlock()
}

func (g *Gateway) markDirty() {
	select {
	case g.dirty <- struct{}{}:
	default:
	}
}

func (g *Gateway) stateLoop() {
	defer g.wg.Done()
	for {
		select {
		case <-g.done:
			g.writeState()
			return
		case <-g.dirty:
			t := time.NewTimer(500 * time.Millisecond)
			select {
			case <-t.C:
			case <-g.done:
				t.Stop()
				g.writeState()
				return
			}
			g.writeState()
		}
	}
}

func (g *Gateway) writeState() {
	if g.stateDir == "" {
		return
	}
	g.mu.Lock()
	st := gatewayState{DayKey: g.dayKey, MonthKey: g.monthKey, DailyUse: map[string]int{}}
	for k, v := range g.dailyUse {
		st.DailyUse[k] = v
	}
	g.mu.Unlock()

	g.meterMu.Lock()
	st.TokensIn, st.TokensOut = g.tokensIn, g.tokensOut
	st.CostUSD, st.CallsTotal, st.CallsCache = g.costUSD, g.callsTotal, g.callsCache
	g.meterMu.Unlock()

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	if err := atomicWriteFile(g.statePath(), data, 0o600); err != nil {
		log.Printf("gateway: state write failed: %v", err)
	}
}

func (g *Gateway) Close() {
	g.closeOne.Do(func() { close(g.done) })
	g.wg.Wait()
}
