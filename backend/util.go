package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
// Everything in this app is calendar-day arithmetic in the *user's* timezone.
// parseDate and todayIn therefore always agree on a location; mixing a UTC
// parse with a local "today" silently shifts every duration by the UTC offset.

// todayIn returns midnight of the current day in loc.
func todayIn(loc *time.Location) time.Time {
	n := time.Now().In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
}

func today() time.Time { return todayIn(time.Local) }

func dateStr(t time.Time) string { return t.Format(dateLayout) }

// parseDateIn parses YYYY-MM-DD as midnight in loc.
func parseDateIn(s string, loc *time.Location) (time.Time, bool) {
	t, err := time.ParseInLocation(dateLayout, strings.TrimSpace(s), loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func parseDate(s string) (time.Time, bool) { return parseDateIn(s, time.Local) }

// daysBetween counts calendar days from -> to, immune to DST transitions
// because it compares dates rather than instants.
func daysBetween(from, to time.Time) int {
	a := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	b := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	return int(b.Sub(a).Hours() / 24)
}

// loadLocation resolves an IANA timezone name, falling back to the server's
// local zone. time/tzdata is embedded (see main.go) so this works without the
// host having a zoneinfo database.
func loadLocation(name string) *time.Location {
	if strings.TrimSpace(name) == "" {
		return time.Local
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.Local
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
