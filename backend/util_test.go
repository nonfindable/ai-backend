package main

import (
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCapitalizeKeepsUTF8Intact(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"fitnes":  "Fitnes",
		"guitar":  "Guitar",
		"гейм":    "Гейм",
		"геймер":  "Геймер",
		"спорт":   "Спорт",
		"бизнес":  "Бизнес",
		"o'yin":   "O'yin",
		"шахматы": "Шахматы",
	}
	for in, want := range cases {
		got := capitalize(in)
		if got != want {
			t.Errorf("capitalize(%q) = %q, want %q", in, got, want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("capitalize(%q) produced invalid UTF-8: %q", in, got)
		}
	}
}

func TestContainsStemRequiresWordStart(t *testing.T) {
	cases := []struct {
		hay, stem string
		want      bool
	}{
		{"i want to get fit", "fit", true},
		{"learn about profit margins", "fit", false}, // "profit"
		{"the benefit of it", "fit", false},
		{"my new outfit", "fit", false},
		{"i want to write code", "code", true},
		{"how to decode this", "code", false},
		{"data analytics", "data", true},
		{"хочу учить английский", "английск", true}, // stem matches inflection
		{"изучаю программирование", "программир", true},
		{"dushanba va seshanba", "shanba", false}, // Saturday must not match Monday
		{"shanba kuni", "shanba", true},
		{"yakshanba", "shanba", false},
		{"", "fit", false},
		{"fit", "", false},
	}
	for _, c := range cases {
		if got := containsStem(c.hay, c.stem); got != c.want {
			t.Errorf("containsStem(%q, %q) = %v, want %v", c.hay, c.stem, got, c.want)
		}
	}
}

// parseDate and today must agree on a location, or every duration computed
// between them is off by the UTC offset.
func TestParseDateAndTodayShareLocation(t *testing.T) {
	for _, zone := range []string{"Asia/Tashkent", "UTC", "America/New_York", "America/Los_Angeles", "Pacific/Auckland"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatalf("LoadLocation(%s): %v", zone, err)
		}
		base := todayIn(loc)
		for _, days := range []int{1, 7, 14, 27, 28, 70} {
			target := base.AddDate(0, 0, days)
			parsed, ok := parseDateIn(dateStr(target), loc)
			if !ok {
				t.Fatalf("parseDateIn(%s) failed", dateStr(target))
			}
			if got := daysBetween(base, parsed); got != days {
				t.Errorf("%s: %d days out measured as %d", zone, days, got)
			}
			if gotWeeks := daysBetween(base, parsed)/7 + 1; gotWeeks != days/7+1 {
				t.Errorf("%s: %d days -> %d weeks, want %d", zone, days, gotWeeks, days/7+1)
			}
		}
	}
}

func TestDaysBetweenSurvivesDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	// Spring forward 2026-03-08 and fall back 2026-11-01.
	for _, c := range []struct{ from, to string }{
		{"2026-03-06", "2026-03-10"},
		{"2026-10-30", "2026-11-03"},
	} {
		a, _ := parseDateIn(c.from, loc)
		b, _ := parseDateIn(c.to, loc)
		if got := daysBetween(a, b); got != 4 {
			t.Errorf("daysBetween(%s, %s) = %d, want 4", c.from, c.to, got)
		}
	}
}

func TestParseIntRejectsJunk(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"42", 42, true},
		{" 7 ", 7, true},
		{"-5", -5, true},
		{"12abc", 0, false},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, c := range cases {
		got, ok := parseInt(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseInt(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSafeFilenameTokenNeutralizesUserText(t *testing.T) {
	cases := []struct{ in, want string }{
		{`xx"; filename="owned.txt`, "xx-filename-owned.txt"},
		{"../../etc/passwd", "etc-passwd"},
		{"Тайский язык", "plan"},
		{"a\nb", "a-b"},
		{"", "plan"},
		{"IELTS", "IELTS"},
	}
	for _, c := range cases {
		got := safeFilenameToken(c.in)
		if got != c.want {
			t.Errorf("safeFilenameToken(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.ContainsAny(got, `"\/;`+"\r\n") {
			t.Errorf("safeFilenameToken(%q) = %q still contains a header-breaking character", c.in, got)
		}
	}
}

func TestFirstNonEmptyTrims(t *testing.T) {
	if got := firstNonEmpty("   ", "\t", " x "); got != "x" {
		t.Errorf("firstNonEmpty = %q, want %q", got, "x")
	}
	if got := firstNonEmpty("", "  "); got != "" {
		t.Errorf("firstNonEmpty = %q, want empty", got)
	}
}

func TestKeyedMutexSerializesPerKeyAndCleansUp(t *testing.T) {
	km := newKeyedMutex()
	// Pre-built so the map itself is never written concurrently; each counter is
	// guarded only by its own key's lock.
	counters := map[string]*int{"a": new(int), "b": new(int)}
	var wg sync.WaitGroup
	for _, key := range []string{"a", "b"} {
		for i := 0; i < 200; i++ {
			wg.Add(1)
			go func(k string) {
				defer wg.Done()
				unlock := km.Lock(k)
				defer unlock()
				*counters[k]++ // a lost update here means the key did not serialize
			}(key)
		}
	}
	wg.Wait()
	for key, c := range counters {
		if *c != 200 {
			t.Errorf("key %s: got %d increments, want 200", key, *c)
		}
	}
	km.mu.Lock()
	defer km.mu.Unlock()
	if len(km.m) != 0 {
		t.Errorf("keyedMutex leaked %d entries", len(km.m))
	}
}
