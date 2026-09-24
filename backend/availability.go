package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- study availability ----
//
// A deadline says how much CALENDAR time exists. It says nothing about how much
// of that time the learner can actually study, and conflating the two is how a
// plan ends up sized against thousands of hours that were never available.
// StudyAvailability is the separate, explicitly-collected answer to "when, and
// for how long?", and it is the only thing feasibility arithmetic may use.
//
// It is deliberately three-valued about what is known:
//
//	HasDays()  the weekdays are known
//	HasTime()  a weekly total (or enough per-day detail to derive one) is known
//	Complete() both, so capacity can be computed rather than guessed
//
// Nothing defaults silently. planAvailability still has to produce something
// for the scheduler, but it reports whether that something was assumed, and the
// confirmation gate refuses to present arithmetic built on an assumption.

// weekdayOrder is the canonical Mon-to-Sun ordering used everywhere day codes
// are stored, so two equal availabilities compare equal as strings.
var weekdayOrder = map[string]int{"Mon": 0, "Tue": 1, "Wed": 2, "Thu": 3, "Fri": 4, "Sat": 5, "Sun": 6}

// weekdayCode maps a time.Weekday onto the stored code.
func weekdayCode(d time.Weekday) string {
	for _, wd := range weekdayTokens {
		if wd.day == d {
			return wd.code
		}
	}
	return ""
}

// sortDayCodes puts canonical codes in Mon-to-Sun order.
func sortDayCodes(days []string) {
	sort.SliceStable(days, func(i, j int) bool { return weekdayOrder[days[i]] < weekdayOrder[days[j]] })
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func (a StudyAvailability) clone() StudyAvailability {
	c := a
	c.Days = append([]string(nil), a.Days...)
	c.PerDay = append([]DayAvailability(nil), a.PerDay...)
	return c
}

// Bounds on a stated commitment. The weekly maximum matches the 40 hours the
// intake answers have always clamped to; the per-day maximum stops one day
// absorbing an entire week.
const (
	minWeeklyMinutes = 15
	maxWeeklyMinutes = 40 * 60
	minDayMinutes    = 15
	maxDayMinutes    = 12 * 60
)

// normalize canonicalizes day codes, drops per-day entries for days that are
// not actually available, clamps every figure, and derives the weekly total
// from per-day minutes whenever those cover the whole week. Deriving here --
// rather than asking the model for a number it may or may not add up -- is what
// makes "Mon/Wed/Fri, an hour each" mean exactly 180 minutes every time.
func (a *StudyAvailability) normalize() {
	days := []string{}
	seen := map[string]bool{}
	for _, raw := range a.Days {
		for _, c := range detectDays(raw) {
			if !seen[c] {
				seen[c] = true
				days = append(days, c)
			}
		}
	}

	perDay := map[string]int{}
	for _, pd := range a.PerDay {
		codes := detectDays(pd.Weekday)
		if len(codes) == 0 || pd.Minutes <= 0 {
			continue
		}
		code := codes[0]
		perDay[code] = clamp(pd.Minutes, minDayMinutes, maxDayMinutes)
		// A day named only in the per-day breakdown is still an available day.
		if !seen[code] {
			seen[code] = true
			days = append(days, code)
		}
	}
	sortDayCodes(days)
	a.Days = days

	a.PerDay = nil
	for _, code := range days {
		if m, ok := perDay[code]; ok {
			a.PerDay = append(a.PerDay, DayAvailability{Weekday: code, Minutes: m})
		}
	}

	// Per-day detail that covers every available day IS the weekly total.
	if len(a.PerDay) > 0 && len(a.PerDay) == len(days) {
		total := 0
		for _, pd := range a.PerDay {
			total += pd.Minutes
		}
		a.WeeklyMinutes = total
	}
	if a.WeeklyMinutes > 0 {
		a.WeeklyMinutes = clamp(a.WeeklyMinutes, minWeeklyMinutes, maxWeeklyMinutes)
	} else {
		a.WeeklyMinutes = 0
	}
}

// HasDays reports whether the available weekdays are known.
func (a StudyAvailability) HasDays() bool { return len(a.Days) > 0 }

// HasTime reports whether a weekly study total is known.
func (a StudyAvailability) HasTime() bool { return a.WeeklyMinutes > 0 }

// Complete reports whether capacity can be computed instead of assumed.
func (a StudyAvailability) Complete() bool { return a.HasDays() && a.HasTime() }

// HoursPerWeek is the rounded mirror kept for the existing API fields. It is a
// display value: every calculation uses WeeklyMinutes.
func (a StudyAvailability) HoursPerWeek() int {
	if a.WeeklyMinutes <= 0 {
		return 0
	}
	return maxInt(1, (a.WeeklyMinutes+30)/60)
}

// MinutesFor returns the cap for one weekday: its own stated limit when the
// user gave one, otherwise an even share of the weekly budget.
func (a StudyAvailability) MinutesFor(code string) int {
	for _, pd := range a.PerDay {
		if pd.Weekday == code {
			return pd.Minutes
		}
	}
	if len(a.Days) == 0 || a.WeeklyMinutes <= 0 {
		return 0
	}
	return a.WeeklyMinutes / len(a.Days)
}

// missingAvailability names the one thing still needed, or "" when nothing is.
// It drives the follow-up question, so a user who said "Monday and Saturday" is
// asked only how long for -- never asked to repeat the days.
func missingAvailability(a StudyAvailability) string {
	switch {
	case !a.HasDays() && !a.HasTime():
		return "both"
	case !a.HasDays():
		return "days"
	case !a.HasTime():
		return "time"
	}
	return ""
}

// mergeAvailability layers a newly-parsed answer over what is already known
// without blanking anything: a reply that only names days keeps the weekly
// total, and a reply that only names a total keeps the days.
func mergeAvailability(base, add StudyAvailability) StudyAvailability {
	out := base.clone()
	if add.HasDays() {
		out.Days = append([]string(nil), add.Days...)
		// The day set changed, so per-day detail attached to the old set is no
		// longer trustworthy; only keep entries for days that survived.
		kept := []DayAvailability{}
		for _, pd := range out.PerDay {
			if containsStr(out.Days, pd.Weekday) {
				kept = append(kept, pd)
			}
		}
		out.PerDay = kept
	}
	if len(add.PerDay) > 0 {
		out.PerDay = append([]DayAvailability(nil), add.PerDay...)
		// Per-day detail recomputes the total, so drop the stale one and let
		// normalize derive a figure that actually adds up.
		out.WeeklyMinutes = 0
	}
	if add.WeeklyMinutes > 0 {
		out.WeeklyMinutes = add.WeeklyMinutes
		// A fresh weekly total overrides a per-day breakdown that contradicts
		// it, unless the breakdown arrived in the very same answer.
		if len(add.PerDay) == 0 {
			sum := 0
			for _, pd := range out.PerDay {
				sum += pd.Minutes
			}
			if sum != add.WeeklyMinutes {
				out.PerDay = nil
			}
		}
	}
	out.normalize()
	return out
}

// availabilityFingerprint renders availability as a comparable string, so a
// change to any part of it is detectable without a deep comparison.
func availabilityFingerprint(a StudyAvailability) string {
	parts := make([]string, 0, len(a.PerDay)+2)
	parts = append(parts, strings.Join(a.Days, ","), itoa(a.WeeklyMinutes))
	for _, pd := range a.PerDay {
		parts = append(parts, pd.Weekday+":"+itoa(pd.Minutes))
	}
	return strings.Join(parts, "|")
}

// ---- capacity ----

// availableStudyMinutes counts the study minutes the learner's OWN availability
// yields between two dates. This is the number feasibility is allowed to use:
// not (deadline - today), and not a default nobody chose.
//
// It walks the calendar in 7-day blocks from start, adds each available day's
// minutes, and caps every block at the weekly budget -- so a week clipped by
// the deadline contributes only the days that actually fall inside it.
func availableStudyMinutes(a StudyAvailability, start, end time.Time) (int, bool) {
	if !a.Complete() {
		return 0, false
	}
	days := daysBetween(start, end)
	if days < 0 {
		return 0, true
	}
	dayset := map[string]bool{}
	for _, d := range a.Days {
		dayset[d] = true
	}
	total := 0
	for offset := 0; offset <= days; {
		weekTotal := 0
		for i := 0; i < 7 && offset <= days; i, offset = i+1, offset+1 {
			d := start.AddDate(0, 0, offset)
			code := weekdayCode(d.Weekday())
			if !dayset[code] {
				continue
			}
			weekTotal += a.MinutesFor(code)
		}
		total += minInt(weekTotal, a.WeeklyMinutes)
	}
	return total, true
}

// ---- parsing free text into availability ----
//
// The backend does this arithmetic, not the model. "Monday, Wednesday and
// Friday, an hour each" is exactly 180 minutes a week, and a deterministic
// parser gets that right every time while a model gets it right most of the
// time. The model still reads intent and language; the numbers are ours.

// reClauseSplit breaks an answer into the units a single fact lives in.
// Cyrillic cannot use \b (RE2 defines it over ASCII only), so the localized
// conjunctions are matched with explicit surrounding whitespace.
var reClauseSplit = regexp.MustCompile(`(?i)[,;.\n]+|\s+and\s+|\s+va\s+|\s+и\s+|\s+plus\s+`)

// reQtyUnit matches "<quantity> <time unit>" in English, Russian and Uzbek.
// The quantity is either digits or a spelled-out number; the longest unit
// spellings come first so "minutes" is not clipped to "min".
//
// The unit is terminated by an explicit "not a letter or digit, or end of
// string" rather than by \b. RE2 defines \b over ASCII word characters only, so
// a trailing \b never matches after a Cyrillic letter — "2 часа" simply would
// not be recognized. The terminator is consumed, which is harmless here because
// only the two capture groups are used.
var reQtyUnit = regexp.MustCompile(`(?i)([0-9]+(?:[.,][0-9]+)?|[\p{L}’'‘ʼ]+)[\s-]*(hours?|hrs?|hr|h|minutes?|mins?|min|m|часов|часам|часа|часу|час|ч|минуты|минуту|минута|минут|мин|soatlik|soat|daqiqa|minut)(?:[^\p{L}\p{N}]|$)`)

// reRuDistributive matches the Russian "по" that means "each" ("по одному
// часу", "по 2 часа"). It needs a whole-word match: strings.Contains("по")
// fires inside "потом" and "помощь".
var reRuDistributive = regexp.MustCompile(`(^|\s)по\s`)

// numberWords covers the spelled-out quantities people actually use. Uzbek
// "o'n" (ten) is listed with its apostrophe variants but bare "on" deliberately
// is NOT: it is an English preposition and would fire on "on Monday".
var numberWords = map[string]float64{
	"a": 1, "an": 1, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11,
	"twelve": 12, "half": 0.5, "couple": 2,

	// Russian inflects the numeral to agree with the case the unit takes
	// ("по одному часу", "по два часа"), so the oblique forms are listed too.
	"один": 1, "одна": 1, "одну": 1, "одному": 1, "одного": 1,
	"два": 2, "две": 2, "двум": 2, "двух": 2,
	"три": 3, "трём": 3, "трем": 3, "трёх": 3, "трех": 3,
	"четыре": 4, "четырём": 4, "четырем": 4, "пять": 5, "пяти": 5,
	"шесть": 6, "шести": 6, "семь": 7, "семи": 7, "восемь": 8, "восьми": 8,
	"девять": 9, "девяти": 9, "десять": 10, "десяти": 10,
	"пол": 0.5, "полтора": 1.5,

	"bir": 1, "ikki": 2, "uch": 3, "to'rt": 4, "to‘rt": 4, "tort": 4, "besh": 5,
	"olti": 6, "yetti": 7, "sakkiz": 8, "to'qqiz": 9, "to‘qqiz": 9,
	"o'n": 10, "o‘n": 10, "yarim": 0.5,
}

var hourUnits = map[string]bool{
	"hour": true, "hours": true, "hr": true, "hrs": true, "h": true,
	"час": true, "часа": true, "часов": true, "часу": true, "часам": true, "ч": true,
	"soat": true, "soatlik": true,
}

// halfHourPhrases are collapsed before matching, because "half an hour" would
// otherwise match on "an hour" and come out as sixty minutes.
var halfHourPhrases = strings.NewReplacer(
	"half an hour", "30 minutes",
	"half hour", "30 minutes",
	"полчаса", "30 минут",
	"пол часа", "30 минут",
	"yarim soat", "30 daqiqa",
)

var weekMarkers = []string{
	"per week", "a week", "each week", "every week", "/week", "weekly", "week total",
	"в неделю", "неделю", "еженедельн",
	"haftasiga", "haftada", "haftalik",
}

// dayMarkers mark a duration as a PER-DAY rate. Every spelling of "each day"
// has to be here, and it has to agree with dayPhraseSets: "everyday" was listed
// there as meaning all seven days but was missing here, so "everyday 2hours"
// set a seven-day week and then filed the two hours as the weekly total — two
// hours a week instead of fourteen. The slash forms matter for the same reason:
// "5h/day" was read as five hours a WEEK.
var dayMarkers = []string{
	"each", "per day", "a day", "every day", "everyday", "daily", "each day",
	"per session", "/day", "/d ", "a day.", "x day", "per-day",
	"в день", "каждый день", "ежедневн", "/день",
	"kuniga", "har kuni", "kunlik", "har biri", "har bir", "/kun",
}

// ---- day phrases ----
//
// detectDays only recognizes NAMED weekdays. People just as often describe
// their week as a shape: "every day", "7 days a week", "weekdays", "weekends".
// Those used to produce no day set at all, which silently discarded the
// duration attached to them — "2 hours each day, 7 days a week" came out as two
// hours a WEEK instead of fourteen, and the plan was then sized against a
// seventh of the time the learner actually had.

var allWeekdays = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// dayPhraseSets maps a phrase naming a whole set of days onto that set.
var dayPhraseSets = []struct {
	keys []string
	days []string
}{
	{[]string{"weekday", "weekdays", "в будни", "по будням", "будни", "будн",
		"ish kunlari", "ish kuni", "ishchi kunlari"},
		[]string{"Mon", "Tue", "Wed", "Thu", "Fri"}},
	{[]string{"weekend", "weekends", "выходны", "по выходным",
		"dam olish kunlari", "dam olish kuni", "hafta oxiri"},
		[]string{"Sat", "Sun"}},
	{[]string{"every day", "each day", "everyday", "daily", "all week",
		"7 days a week", "seven days a week", "every single day",
		"каждый день", "ежедневн", "все дни", "7 дней в неделю",
		"har kuni", "har kun", "kunda", "hafta davomida"},
		allWeekdays},
}

// reDaysCount reads "<n> days a week". The week marker is required separately,
// so "in 5 days" (a deadline) can never be mistaken for a day count.
var reDaysCount = regexp.MustCompile(`(?i)([0-9]+|[\p{L}’'‘ʼ]+)[\s-]*(days?|дней|дня|дни|kunlari|kun)(?:[^\p{L}\p{N}]|$)`)

// detectDayPhrases returns the day set a phrase names, and how many days a
// "<n> days a week" answer states. count is reported even when the days
// themselves are unknown: knowing there are five of them makes a weekly total
// derivable, while which five is still a fair question to ask.
func detectDayPhrases(lower string) (days []string, count int) {
	for _, set := range dayPhraseSets {
		if hasMarker(lower, set.keys) {
			days = append([]string(nil), set.days...)
			break
		}
	}
	// "<n> days a week", and also "on three days". The qualifier is required so
	// that "my exam is in 5 days" — a deadline — is never read as a day count.
	if hasMarker(lower, weekMarkers) || strings.Contains(lower, " on ") || strings.HasPrefix(lower, "on ") {
		if m := reDaysCount.FindStringSubmatch(lower); m != nil {
			if q, ok := quantity(m[1]); ok && q >= 1 && q <= 7 {
				count = int(q)
				if count == 7 && len(days) == 0 {
					days = append([]string(nil), allWeekdays...)
				}
			}
		}
	}
	if len(days) > 0 && count == 0 {
		count = len(days)
	}
	return days, count
}

// moneyMarkers identify an answer that is about money, so its digits are never
// read as a quantity of time.
var moneyMarkers = []string{
	"$", "€", "£", "₽", "₴", "usd", "eur", "dollar", "euro", "budget", "free",
	"руб", "доллар", "евро", "сум", "сумм", "бюджет", "бесплат", "тысяч",
	"so'm", "so‘m", "som", "byudjet", "bepul", "ming so",
}

// looksLikeMoney reports whether the text is talking about cost.
func looksLikeMoney(s string) bool {
	return hasMarker(strings.ToLower(s), moneyMarkers)
}

func hasMarker(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// hasEachMarker covers the "one hour each" sense in all three languages,
// including the Russian distributive "по" which needs a word-boundary match.
func hasEachMarker(s string) bool {
	return hasMarker(s, dayMarkers) || reRuDistributive.MatchString(s)
}

// quantity resolves the matched quantity token to a number.
func quantity(tok string) (float64, bool) {
	t := strings.ToLower(strings.TrimSpace(tok))
	if t == "" {
		return 0, false
	}
	if t[0] >= '0' && t[0] <= '9' {
		n, err := strconv.ParseFloat(strings.Replace(t, ",", ".", 1), 64)
		if err != nil || n <= 0 {
			return 0, false
		}
		return n, true
	}
	n, ok := numberWords[t]
	return n, ok && n > 0
}

// firstDurationMinutes returns the first plausible duration in s.
func firstDurationMinutes(s string) (int, bool) {
	for _, m := range reQtyUnit.FindAllStringSubmatch(s, -1) {
		numeric := m[1] != "" && m[1][0] >= '0' && m[1][0] <= '9'
		unit := strings.ToLower(m[2])
		// A one-letter unit is only trustworthy after digits: "a m" is not
		// "a minute", but "45 m" is.
		if !numeric && len([]rune(unit)) <= 1 {
			continue
		}
		q, ok := quantity(m[1])
		if !ok {
			continue
		}
		mins := int(q + 0.5)
		if hourUnits[unit] {
			mins = int(q*60 + 0.5)
		}
		// The ceiling here is a week of wall-clock, not a day's worth. This
		// function reads WEEKLY totals as well as per-day ones — "20 hours a
		// week" is 1200 minutes and perfectly ordinary — and a per-day cap
		// would have silently dropped every weekly figure above twelve hours.
		// Anything genuinely out of range is clamped by normalize, which knows
		// whether it is looking at a day or a week; only the absurd is rejected.
		if mins >= minDayMinutes && mins <= 7*24*60 {
			return mins, true
		}
	}
	return 0, false
}

// ---- statements vs. state ----
//
// Parsing availability is TWO jobs, and conflating them is what produced
// "everyday 2hours" = 2 hours a week.
//
//  1. EXTRACT what one message said, preserving its unit. "2 hours per day" is
//     a RATE; "6 hours a week" is a TOTAL. Flattening both into one integer
//     loses the only thing that distinguishes them.
//  2. RESOLVE that statement against what is already known. A rate becomes a
//     weekly total only once the number of days is known — and those days are
//     usually already in the session, not repeated in the message. "5h/day"
//     said to someone already studying seven days is 35 hours a week; the
//     learner should not have to restate their week to change its length.
//
// availabilityStatement is the output of (1). It is never stored.
type availabilityStatement struct {
	// Days is the week this message names, either by weekday or by phrase.
	Days []string
	// DayCount is how many days it states without naming them ("5 days a
	// week", "on three days"). A weekly total is derivable from a rate and a
	// count even when WHICH days is still an open question.
	DayCount int
	// PerDay holds day-specific durations ("Monday 2 hours, Saturday 3 hours").
	PerDay map[string]int
	// UniformDay is a per-day RATE ("2 hours each day", "5h/day").
	UniformDay int
	// Weekly is a per-week TOTAL ("6 hours a week").
	Weekly int
	// SawDuration records that some duration was stated, so the bare-number
	// fallback knows to stay out of the way.
	SawDuration bool
	// BareWeekly is the fallback reading of an answer with no time unit at all
	// ("about 6"), used only where the reply is known to be about availability.
	BareWeekly int
}

// empty reports whether the message said nothing at all about availability.
func (st availabilityStatement) empty() bool {
	return len(st.Days) == 0 && st.DayCount == 0 && len(st.PerDay) == 0 &&
		st.UniformDay == 0 && st.Weekly == 0 && st.BareWeekly == 0
}

// parseAvailabilityStatement extracts what a message says about availability,
// keeping per-day and per-week apart. It resolves nothing and assumes nothing.
func parseAvailabilityStatement(text string) availabilityStatement {
	st := availabilityStatement{PerDay: map[string]int{}}
	if strings.TrimSpace(text) == "" {
		return st
	}
	lower := halfHourPhrases.Replace(strings.ToLower(text))

	// Named weekdays always win over a phrase: in "Mon, Wed and Fri, 1 hour
	// each day" the "each day" means each of those three, not all seven.
	phraseDays, phraseCount := detectDayPhrases(lower)
	if named := detectDays(lower); len(named) > 0 {
		st.Days = named
		st.DayCount = len(named)
	} else {
		st.Days = phraseDays
		st.DayCount = phraseCount
	}
	// A message that counts its days ("on three days") makes a bare duration a
	// per-day rate rather than a weekly total.
	countPhrase := len(st.Days) == 0 && phraseCount > 0

	weekly, bare := 0, 0
	for _, clause := range reClauseSplit.Split(lower, -1) {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		dur, ok := firstDurationMinutes(clause)
		if !ok {
			continue
		}
		st.SawDuration = true
		switch days := detectDays(clause); {
		case hasMarker(clause, weekMarkers):
			weekly = dur
		case hasEachMarker(clause):
			st.UniformDay = dur
		case len(days) == 1:
			st.PerDay[days[0]] = dur
		case countPhrase:
			st.UniformDay = dur
		default:
			// A lone figure with no unit of its own reads as the weekly
			// commitment, which is what the interview asks for.
			bare = dur
		}
	}
	st.Weekly = firstPositive(weekly, bare)

	// The no-unit fallback, computed here but applied only where the caller
	// says this reply is answering the availability question.
	if !st.SawDuration && looksLikeBareHours(lower) {
		if h, ok := parseHoursPerWeek(text); ok {
			st.BareWeekly = clamp(h*60, minWeeklyMinutes, maxWeeklyMinutes)
		}
	}
	return st
}

// scoreMarkers are the ways a number means a LEVEL rather than a duration.
var scoreMarkers = []string{
	"band", "level", "score", "+", "ielts", "toefl", "cefr",
	"балл", "уровен", "ball", "daraja",
}

// looksLikeBareHours reports whether a message with no time unit is plausibly
// just a number of hours ("6", "about 6").
//
// The fallback has to be narrow, because a stray digit anywhere else is
// indistinguishable from an answer: "Keep target 7+ and extend deadline to fit
// needed hours" was read as seven hours a week, and "band 7" would have been
// too. A real answer to "how much time?" is short and carries no score.
func looksLikeBareHours(lower string) bool {
	if looksLikeMoney(lower) || hasMarker(lower, scoreMarkers) {
		return false
	}
	return len(strings.Fields(lower)) <= 4
}

func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// resolveAvailability applies a statement to what is already known.
//
// This is where the latest correction wins. A message that names days replaces
// the day set; a message that gives a rate re-derives the weekly total from the
// days in force; a message that gives a weekly total drops a per-day breakdown
// that no longer describes it. The result is always internally consistent,
// because normalize derives WeeklyMinutes from PerDay whenever the breakdown
// covers the whole week — there is exactly one source of truth for the total.
//
// allowBareNumber is true only when the caller knows this reply is answering
// the availability question, since "about 6" is only hours per week in that
// context.
func resolveAvailability(current StudyAvailability, st availabilityStatement, allowBareNumber bool) StudyAvailability {
	out := current.clone()
	out.normalize()

	if len(st.Days) > 0 && strings.Join(st.Days, ",") != strings.Join(out.Days, ",") {
		// A different week: any per-day detail belonged to the old one.
		out.Days = append([]string(nil), st.Days...)
		out.PerDay = nil
	}

	switch {
	case len(st.PerDay) > 0:
		// Day-specific durations replace the breakdown outright.
		for code := range st.PerDay {
			if !containsStr(out.Days, code) {
				out.Days = append(out.Days, code)
			}
		}
		sortDayCodes(out.Days)
		out.PerDay = nil
		for _, code := range out.Days {
			if mins, ok := st.PerDay[code]; ok {
				out.PerDay = append(out.PerDay, DayAvailability{Weekday: code, Minutes: mins})
			}
		}
		if len(out.PerDay) < len(out.Days) {
			// Only part of the week was described, so the total is no longer
			// derivable and must not keep a value that contradicts it.
			out.WeeklyMinutes = 0
		}

	case st.UniformDay > 0:
		switch {
		case len(out.Days) > 0:
			// THE FIX for "5h/day": the days are already known, so the rate
			// resolves against them without the learner repeating their week.
			out.PerDay = nil
			for _, code := range out.Days {
				out.PerDay = append(out.PerDay, DayAvailability{Weekday: code, Minutes: st.UniformDay})
			}
		case st.DayCount > 0:
			out.PerDay = nil
			out.WeeklyMinutes = st.UniformDay * st.DayCount
		default:
			// A rate with no idea how many days it applies to derives nothing,
			// and must not be quietly rounded into a weekly figure.
		}

	case st.Weekly > 0:
		out.WeeklyMinutes = st.Weekly
		out.PerDay = nil // an explicit total makes any previous split unknown

	case allowBareNumber && st.BareWeekly > 0:
		out.WeeklyMinutes = st.BareWeekly
		out.PerDay = nil
	}

	out.normalize()
	return out
}

// parseAvailabilityText reads a message in isolation. A figure must carry a
// time unit and a day must be named or implied; nothing is inferred from a bare
// number, and nothing is assumed about a week it was not told.
func parseAvailabilityText(text string) StudyAvailability {
	return resolveAvailability(StudyAvailability{}, parseAvailabilityStatement(text), false)
}

// parseAvailabilityAnswer reads a reply KNOWN to be answering the availability
// question, so a bare count with no unit ("about 6") may be read as hours per
// week. A sum of money never is: "$50+" is a budget, not forty hours.
func parseAvailabilityAnswer(text string) StudyAvailability {
	return resolveAvailability(StudyAvailability{}, parseAvailabilityStatement(text), true)
}
