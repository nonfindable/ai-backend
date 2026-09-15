package main

import (
	"strings"
	"time"
)

// Localization helpers. The app supports English, Russian, and Uzbek.
// - In LIVE mode, withLang() tells the ChatGPT model which language to answer in.
// - In MOCK mode, tr() picks the right hard-coded string so the no-key demo
//   also runs fully in the chosen language.

func normLang(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "ru", "uz", "en":
		return strings.ToLower(strings.TrimSpace(lang))
	default:
		return "en"
	}
}

func langName(lang string) string {
	switch normLang(lang) {
	case "ru":
		return "Russian"
	case "uz":
		return "Uzbek"
	default:
		return "English"
	}
}

// tr returns the string for the active language.
func tr(lang, en, ru, uz string) string {
	switch normLang(lang) {
	case "ru":
		return ru
	case "uz":
		return uz
	default:
		return en
	}
}

// withLang appends a language directive to a system prompt (live ChatGPT mode).
// JSON keys and enum values stay in English so parsing is unaffected.
func withLang(base, lang string) string {
	return base + "\n\nIMPORTANT: Write ALL user-facing text — questions, overview, " +
		"decline message, assessment, feasibility, phase titles and summaries, milestone " +
		"titles, todo titles, and setup names/rationales — in " + langName(lang) + ". " +
		"Keep the JSON keys and enum values (frequency, priority, category) in English."
}

// ---- weekdays ----
//
// Day names have to be recognized in all three supported languages: an intake
// answer of "пн, ср" or "dushanba" is as valid as "Mon". The table is an
// ordered slice rather than a map so detection returns Mon→Sun deterministically
// instead of in Go's randomized map order.
//
// Matching uses containsStem (word-start boundary, open-ended tail) so Russian
// and Uzbek stems match their inflections, and so "shanba" (Saturday) does not
// match inside "dushanba" or "yakshanba".
var weekdayTokens = []struct {
	code string
	day  time.Weekday
	keys []string
}{
	{"Mon", time.Monday, []string{"mon", "понедельник", "пн", "dushanba", "dush"}},
	{"Tue", time.Tuesday, []string{"tue", "вторник", "вт", "seshanba", "sesh"}},
	{"Wed", time.Wednesday, []string{"wed", "среда", "сред", "ср", "chorshanba", "chor"}},
	{"Thu", time.Thursday, []string{"thu", "четверг", "чт", "payshanba", "pay"}},
	{"Fri", time.Friday, []string{"fri", "пятниц", "пт", "juma"}},
	{"Sat", time.Saturday, []string{"sat", "суббот", "сб", "shanba"}},
	{"Sun", time.Sunday, []string{"sun", "воскресен", "вс", "yakshanba", "yaksh"}},
}

// detectDays extracts weekday codes from free text, in Mon→Sun order.
func detectDays(text string) []string {
	lower := strings.ToLower(text)
	var out []string
	for _, wd := range weekdayTokens {
		for _, k := range wd.keys {
			if containsStem(lower, k) {
				out = append(out, wd.code)
				break
			}
		}
	}
	return out
}

// parseWeekdaySet turns stored day labels into a weekday set, accepting the
// canonical codes as well as localized names. Falls back to Mon–Fri.
func parseWeekdaySet(days []string) map[time.Weekday]bool {
	set := map[time.Weekday]bool{}
	for _, d := range days {
		lower := strings.ToLower(strings.TrimSpace(d))
		if lower == "" {
			continue
		}
		for _, wd := range weekdayTokens {
			for _, k := range wd.keys {
				if containsStem(lower, k) {
					set[wd.day] = true
					break
				}
			}
		}
	}
	if len(set) == 0 { // default Mon–Fri
		for _, wd := range []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday} {
			set[wd] = true
		}
	}
	return set
}
