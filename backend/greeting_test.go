package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSessionGreeting(t *testing.T) {
	cases := []struct{ lang, name, want string }{
		{"en", "", "Hi there! 👋 I'm start.ai, your learning coach. What would you like to learn? Just tell me in your own words — I'll ask a few quick questions and build a step-by-step plan that fits your schedule."},
		{"ru", "", "Привет! 👋 Я start.ai — ваш помощник в учёбе. Что хотите изучить? Просто напишите своими словами — я задам пару коротких вопросов и составлю пошаговый план под ваш график."},
		{"uz", "", "Salom! 👋 Men start.ai — o'qishdagi yordamchingizman. Nimani o'rganmoqchisiz? O'z so'zlaringiz bilan yozing — men bir nechta qisqa savol beraman va vaqtingizga mos bosqichma-bosqich reja tuzib beraman."},
		{"en", "Shokhboz", "Hi Shokhboz! 👋 I'm start.ai, your learning coach. What would you like to learn? Just tell me in your own words — I'll ask a few quick questions and build a step-by-step plan that fits your schedule."},
		{"ru", "Shokhboz", "Привет, Shokhboz! 👋 Я start.ai — ваш помощник в учёбе. Что хотите изучить? Просто напишите своими словами — я задам пару коротких вопросов и составлю пошаговый план под ваш график."},
		{"uz", "Shokhboz", "Salom, Shokhboz! 👋 Men start.ai — o'qishdagi yordamchingizman. Nimani o'rganmoqchisiz? O'z so'zlaringiz bilan yozing — men bir nechta qisqa savol beraman va vaqtingizga mos bosqichma-bosqich reja tuzib beraman."},
	}
	for _, c := range cases {
		if got := sessionGreeting(c.lang, c.name); got != c.want {
			t.Errorf("sessionGreeting(%q, %q)\n got: %s\nwant: %s", c.lang, c.name, got, c.want)
		}
	}

	// Unknown language falls back to English; a blank name means no name.
	if got := sessionGreeting("de", "   "); !strings.HasPrefix(got, "Hi there! 👋") {
		t.Errorf("fallback greeting = %q", got)
	}
	if got := sessionGreeting("en", "  Ann  "); !strings.HasPrefix(got, "Hi Ann! 👋") {
		t.Errorf("name not trimmed: %q", got)
	}
	if got := sessionGreeting("en", "Ann\nSmith"); !strings.HasPrefix(got, "Hi Ann Smith! 👋") {
		t.Errorf("line break kept in name: %q", got)
	}
	long := strings.Repeat("Ш", 60)
	if n := utf8.RuneCountInString(greetingName(long)); n != maxGreetingNameRunes {
		t.Errorf("name length %d, want capped at %d", n, maxGreetingNameRunes)
	}
}

func TestSessionEndpointGreeting(t *testing.T) {
	ts := newTestServer(t)
	for _, lang := range []string{"en", "ru", "uz"} {
		for _, name := range []string{"", "Shokhboz"} {
			c := &client{t: t, base: ts.URL}
			var out map[string]any
			c.do("POST", "/api/session", map[string]any{"lang": lang, "timezone": "UTC", "name": name}, &out)
			if got, want := out["assistant"], sessionGreeting(lang, name); got != want {
				t.Errorf("lang=%s name=%q: assistant = %v", lang, name, got)
			}
			if out["token"] == nil || out["sessionId"] == nil || out["stage"] != "scope_check" {
				t.Errorf("lang=%s: response shape changed: %v", lang, out)
			}
		}
	}
}
