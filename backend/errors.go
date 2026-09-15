package main

import (
	"errors"
	"log"
	"net/http"
)

// Client-facing errors are a closed set of stable codes with fixed, safe
// messages. Nothing derived from an upstream provider, the filesystem or the
// network ever reaches a client: the message is looked up from the code, never
// taken from err.Error(). The backend previously echoed OpenAI's response body
// straight through, which leaked the account's billing state to any caller.
const (
	codeUnauthorized       = "UNAUTHORIZED"
	codeInvalidRequest     = "INVALID_REQUEST"
	codePlanNotFound       = "PLAN_NOT_FOUND"
	codeSessionNotFound    = "SESSION_NOT_FOUND"
	codeTodoNotFound       = "TODO_NOT_FOUND"
	codeEventNotFound      = "EVENT_NOT_FOUND"
	codeAIUnavailable      = "AI_UNAVAILABLE"
	codeAIRateLimited      = "AI_RATE_LIMITED"
	codeDailyQuotaExceeded = "DAILY_QUOTA_EXCEEDED"
	codeSpendCapReached    = "SPEND_CAP_REACHED"
	codeScheduleConflict   = "SCHEDULE_CONFLICT"
	codeInternal           = "INTERNAL_ERROR"
)

// safeMessages maps each code to the only text a client may see.
var safeMessages = map[string]string{
	codeUnauthorized:       "Authentication required. Send the token from POST /api/session as 'Authorization: Bearer <token>'.",
	codeInvalidRequest:     "The request could not be understood.",
	codePlanNotFound:       "Plan not found.",
	codeSessionNotFound:    "Session not found.",
	codeTodoNotFound:       "Todo not found.",
	codeEventNotFound:      "Scheduled session not found.",
	codeAIUnavailable:      "We couldn't generate your learning plan right now. Please try again shortly.",
	codeAIRateLimited:      "The planner is busy right now. Please try again in a moment.",
	codeDailyQuotaExceeded: "You've reached today's planning limit. Please try again tomorrow.",
	codeSpendCapReached:    "Plan generation is temporarily unavailable. Please try again later.",
	codeScheduleConflict:   "That schedule could not be updated. Please reload and try again.",
	codeInternal:           "Something went wrong on our side.",
}

// apiError is the wire shape: {"error": {"code": ..., "message": ..., "ref": ...}}
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Ref is our own opaque identifier, correlating a client-visible failure
	// with the detailed server log line. It is generated here and carries no
	// upstream, account or infrastructure information.
	Ref string `json:"ref,omitempty"`
}

type apiErrorEnvelope struct {
	Error apiError `json:"error"`
}

func safeMessage(code string) string {
	if m, ok := safeMessages[code]; ok {
		return m
	}
	return safeMessages[codeInternal]
}

// writeAPIError sends a client-safe error. Never pass upstream text here: the
// message comes from the code.
func writeAPIError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, apiErrorEnvelope{Error: apiError{Code: code, Message: safeMessage(code)}})
}

// writeAPIErrorLogging records the internal detail server-side under a fresh
// reference and returns only that reference to the client. Use it wherever a
// real cause exists that operators need but callers must not see.
func writeAPIErrorLogging(w http.ResponseWriter, status int, code string, where string, detail error) {
	ref := newID("err")
	if detail != nil {
		log.Printf("%s: [%s] %s: %v", where, ref, code, detail)
	} else {
		log.Printf("%s: [%s] %s", where, ref, code)
	}
	writeJSON(w, status, apiErrorEnvelope{Error: apiError{Code: code, Message: safeMessage(code), Ref: ref}})
}

// ---- AI failure classification ----
//
// The gateway returns sentinel-wrapped errors so the API can pick a stable code
// without ever inspecting, or forwarding, the provider's own words.

var (
	// errGatewayDisabled means live mode is off (no key, or AI_LIVE=false).
	errGatewayDisabled = errors.New("gateway disabled")
	// errSpendCapReached means the configured monthly USD cap is exhausted.
	errSpendCapReached = errors.New("monthly spend cap reached")
	// errDailyQuota means this user used their per-day call allowance.
	errDailyQuota = errors.New("daily call quota reached")
	// errAIRateLimited means the provider throttled or refused on capacity.
	errAIRateLimited = errors.New("upstream rate limited")
	// errAIUnavailable is every other upstream failure: network, auth, 5xx,
	// malformed payload. The distinction is kept in the log, not the response.
	errAIUnavailable = errors.New("upstream unavailable")
)

// classifyAIError maps an internal AI error onto an HTTP status and a stable
// client code.
func classifyAIError(err error) (int, string) {
	switch {
	case errors.Is(err, errDailyQuota):
		return http.StatusTooManyRequests, codeDailyQuotaExceeded
	case errors.Is(err, errAIRateLimited):
		return http.StatusTooManyRequests, codeAIRateLimited
	case errors.Is(err, errSpendCapReached):
		return http.StatusServiceUnavailable, codeSpendCapReached
	case errors.Is(err, errGatewayDisabled):
		return http.StatusServiceUnavailable, codeAIUnavailable
	case errors.Is(err, errAIUnavailable):
		return http.StatusBadGateway, codeAIUnavailable
	default:
		return http.StatusBadGateway, codeAIUnavailable
	}
}
