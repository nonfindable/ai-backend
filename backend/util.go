package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const dateLayout = "2006-01-02"

// ---- identifiers ----

// newID returns a short, prefixed, collision-resistant identifier.
func newID(prefix string) string { return prefix + "_" + randomHex(8) }

// newToken returns a 256-bit bearer token used to authenticate API calls.
func newToken() string { return randomHex(32) }

// randomHex returns n cryptographically random bytes, hex encoded. A failure
// here would mean handing out guessable identifiers and tokens, so it is fatal
// rather than silently degraded.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// ---- dates ----
//
// THE DATE MODEL
//
// A plan's StartDate, FinishDate, a milestone's TargetDate and an event's Date
// are "floating" local dates in YYYY-MM-DD, and StartTime is a local wall-clock
// HH:MM. The timezone they belong to is ALWAYS the plan's own Plan.Timezone
// (seeded from User.Timezone), never the server's and never the browser's.
//
// That means:
//   - 18:00 on 2026-10-01 means 18:00 where the learner is, on both devices
//   - the only place these become absolute instants is the .ics export, which
//     resolves them against Plan.Timezone and emits UTC
//   - a client must send its IANA zone to POST /api/session and then render
//     these strings as-is; re-interpreting them in the browser's zone shifts
//     sessions by the offset
//
// Every helper below therefore takes an explicit *time.Location. There is
// deliberately no server-local convenience wrapper: one of those silently bound
// deadline parsing to whatever zone the host happened to run in.

// todayIn returns midnight of the current day in loc.
func todayIn(loc *time.Location) time.Time {
	n := time.Now().In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
}

func dateStr(t time.Time) string { return t.Format(dateLayout) }

// parseDateIn parses YYYY-MM-DD as midnight in loc.
func parseDateIn(s string, loc *time.Location) (time.Time, bool) {
	t, err := time.ParseInLocation(dateLayout, strings.TrimSpace(s), loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// validDate reports whether s is a well-formed YYYY-MM-DD calendar date. It is
// pure format validation and deliberately zone-free: which day "2026-12-01"
// refers to is decided later, by the plan's timezone.
func validDate(s string) bool {
	_, err := time.Parse(dateLayout, strings.TrimSpace(s))
	return err == nil
}

// daysBetween counts calendar days from -> to, immune to DST transitions
// because it compares dates rather than instants.
func daysBetween(from, to time.Time) int {
	a := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	b := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	return int(b.Sub(a).Hours() / 24)
}

// loadLocation resolves an IANA timezone name. time/tzdata is embedded (see
// main.go) so this works without the host having a zoneinfo database.
//
// An unresolvable name falls back to UTC, not to the server's local zone, and
// says so in the log. Falling back to time.Local meant a typo'd or missing
// timezone silently scheduled a learner's sessions in whatever zone the host
// machine ran in — a value that differs between a laptop and a container and
// is invisible in the API response.
func loadLocation(name string) *time.Location {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	log.Printf("time: unknown timezone %q; falling back to UTC", name)
	return time.UTC
}

// resolveTimezone validates an IANA name, returning the fallback when it is
// blank or unknown. Callers store the returned name, so a User and a Plan
// always carry a timezone that actually resolves.
func resolveTimezone(name, fallback string) string {
	if n := strings.TrimSpace(name); n != "" {
		if _, err := time.LoadLocation(n); err == nil {
			return n
		}
	}
	if f := strings.TrimSpace(fallback); f != "" {
		if _, err := time.LoadLocation(f); err == nil {
			return f
		}
	}
	return "UTC"
}

// ---- numbers & strings ----

// clamp bounds n to [lo, hi].
func clamp(n, lo, hi int) int {
	if lo > hi {
		lo, hi = hi, lo
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// parseInt parses a base-10 integer, reporting whether the whole string was a
// valid number (unlike a hand-rolled scanner that stops at the first junk byte).
func parseInt(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	return n, true
}

func atoi(s string) int {
	n, _ := parseInt(s)
	return n
}

func itoa(n int) string { return strconv.Itoa(n) }

// firstNonEmpty returns the first argument that is non-blank, trimmed.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// capitalize upper-cases the first *rune*. Slicing the first byte corrupts any
// multi-byte character, which matters because skill names arrive in Russian and
// Uzbek as well as English.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size <= 1 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// isWordChar reports whether r can appear inside a word, for keyword matching.
func isWordChar(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// containsStem reports whether stem occurs in s at the start of a word. Many of
// the skill keywords are stems ("английск", "программир"), so a trailing
// boundary must NOT be required — but a leading one must, otherwise "fit"
// matches "profit" and "code" matches "decode".
func containsStem(s, stem string) bool {
	if stem == "" {
		return false
	}
	for off := 0; off <= len(s)-len(stem); {
		i := strings.Index(s[off:], stem)
		if i < 0 {
			return false
		}
		at := off + i
		if at == 0 {
			return true
		}
		if r, _ := utf8.DecodeLastRuneInString(s[:at]); !isWordChar(r) {
			return true
		}
		off = at + 1
	}
	return false
}

// ---- filenames & headers ----

var reUnsafeFilename = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// rfc5987Encode percent-encodes a value for a Content-Disposition filename*
// parameter. Everything outside the attr-char set of RFC 5987 is escaped, so no
// quote, semicolon or separator can reach the header intact.
func rfc5987Encode(s string) string {
	const attrChars = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			strings.IndexByte(attrChars, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteString(fmt.Sprintf("%%%02X", c))
	}
	return b.String()
}

// safeFilenameToken reduces arbitrary user text to something that cannot break
// out of a Content-Disposition parameter or traverse a path.
func safeFilenameToken(s string) string {
	s = reUnsafeFilename.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-._")
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" {
		return "plan"
	}
	return s
}

// ---- keyed mutex ----
//
// Serializes work per entity (one conversation, one plan) without holding the
// store's global lock across slow work such as an AI call. Entries are
// reference counted so the map does not grow without bound.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*keyedMutexEntry
}

type keyedMutexEntry struct {
	mu   sync.Mutex
	refs int
}

func newKeyedMutex() *keyedMutex { return &keyedMutex{m: map[string]*keyedMutexEntry{}} }

// Lock acquires the lock for key and returns the matching unlock function.
func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	e, ok := k.m[key]
	if !ok {
		e = &keyedMutexEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}
