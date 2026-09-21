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

	// MaxCompletionTokens caps one response. It is sent explicitly because a
	// provider default that is generous for chat is not generous for a plan:
	// Groq's default for gpt-oss-120b is 3072, and a reasoning model spends
	// ~2300 of those thinking, leaving too few for the JSON. The response is
	// then cut off mid-object and comes back either as unparsable text or as a
	// 400 json_validate_failed with an empty failed_generation. 0 omits the
	// field and restores the provider default.
	MaxCompletionTokens int

	// ReasoningEffort is the "reasoning_effort" parameter that reasoning models
	// (gpt-oss, o-series) accept. Sending "low" on gpt-oss-120b cut reasoning
	// from ~2300 tokens to ~220 with no loss of plan quality, which is the
	// difference between fitting and not fitting in a small TPM allowance.
	// Empty omits the field — required for models that reject it.
	ReasoningEffort string

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

		MaxCompletionTokens: getenvInt("AI_MAX_COMPLETION_TOKENS", 8192),
		ReasoningEffort:     getenv("AI_REASONING_EFFORT", ""),

		Port: getenv("PORT", "8080"),
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
	// A bad DEFAULT_TIMEZONE would be inherited by every new user.
	if resolved := resolveTimezone(cfg.DefaultTimezone, "UTC"); resolved != cfg.DefaultTimezone {
		log.Printf("config: DEFAULT_TIMEZONE=%q is not a known IANA zone; using %s", cfg.DefaultTimezone, resolved)
		cfg.DefaultTimezone = resolved
	}
	if cfg.AllowHeaderAuth {
		log.Printf("config: ALLOW_HEADER_AUTH=true accepts an unverified X-User-Id header as identity; do not use this in production")
	}
	return cfg
}
