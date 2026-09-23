package main

import (
	"strings"
	"testing"
	"time"
)

// ---- availability collection ----
//
// The rule these tests defend: a deadline says how much CALENDAR time exists
// and nothing about how much of it the learner can study. Availability is
// collected separately, derived deterministically, and never defaulted.

func availabilityFrom(t *testing.T, text string) StudyAvailability {
	t.Helper()
	return parseAvailabilityText(text)
}

// TEST 7 — per-day durations give the weekly total without a second question.
func TestPerDayAnswerDerivesTheWeeklyTotal(t *testing.T) {
	cases := []string{
		"Monday, Wednesday and Friday, 1 hour each",
		"I can study Monday, Wednesday and Friday for one hour each",
		"пн, ср и пт, по одному часу",
		"dushanba, chorshanba va juma — har biri bir soat",
	}
	for _, in := range cases {
		av := availabilityFrom(t, in)
		if got := strings.Join(av.Days, ","); got != "Mon,Wed,Fri" {
			t.Errorf("%q: days = %q, want Mon,Wed,Fri", in, got)
		}
		if av.WeeklyMinutes != 180 {
			t.Errorf("%q: weeklyMinutes = %d, want 180", in, av.WeeklyMinutes)
		}
		if len(av.PerDay) != 3 {
			t.Errorf("%q: per-day detail = %+v, want three entries", in, av.PerDay)
		}
		if !av.Complete() {
			t.Errorf("%q: availability is not complete, so a redundant question would follow", in)
		}
		if missingAvailability(av) != "" {
			t.Errorf("%q: still reports %q missing", in, missingAvailability(av))
		}
	}
}

// Different amounts on different days add up to the right total.
func TestUnevenPerDayAnswerAddsUp(t *testing.T) {
	av := availabilityFrom(t, "Monday 2 hours, Wednesday 1 hour, Saturday 3 hours")
	if got := strings.Join(av.Days, ","); got != "Mon,Wed,Sat" {
		t.Fatalf("days = %q, want Mon,Wed,Sat", got)
	}
	if av.WeeklyMinutes != 360 {
		t.Errorf("weeklyMinutes = %d, want 360 (120+60+180)", av.WeeklyMinutes)
	}
	want := map[string]int{"Mon": 120, "Wed": 60, "Sat": 180}
	for _, pd := range av.PerDay {
		if want[pd.Weekday] != pd.Minutes {
			t.Errorf("%s = %d minutes, want %d", pd.Weekday, pd.Minutes, want[pd.Weekday])
		}
	}
}

// TEST 8 — a weekly figure plus days is enough, and never becomes 24h/day.
func TestWeeklyFigureWithDaysIsEnough(t *testing.T) {
	for _, in := range []string{
		"Monday, Wednesday and Friday. Six hours per week.",
		"6 hours per week on Mon, Wed and Fri",
		"10 hours a week on Mon Wed Fri",
	} {
		av := availabilityFrom(t, in)
		if got := strings.Join(av.Days, ","); got != "Mon,Wed,Fri" {
			t.Errorf("%q: days = %q, want Mon,Wed,Fri", in, got)
		}
		if !av.Complete() {
			t.Errorf("%q: not complete, want no further question", in)
		}
		if av.WeeklyMinutes > maxWeeklyMinutes {
			t.Errorf("%q: weeklyMinutes = %d — a day of calendar time was mistaken for study time", in, av.WeeklyMinutes)
		}
	}
	if got := availabilityFrom(t, "Monday, Wednesday and Friday. Six hours per week.").WeeklyMinutes; got != 360 {
		t.Errorf("weeklyMinutes = %d, want 360", got)
	}
	if got := availabilityFrom(t, "10 hours a week on Mon Wed Fri").WeeklyMinutes; got != 600 {
		t.Errorf("weeklyMinutes = %d, want 600", got)
	}
}

// A week described by its SHAPE rather than by named weekdays.
//
// Regression: "2 hours each day, 7 days a week" produced 120 minutes a week
// instead of 840. detectDays knows only named weekdays, so the phrase yielded
// no day set, the per-day duration was discarded, and a bare-number fallback
// then read the "2" as the weekly total. A learner with 14 hours a week was
// assessed against two, and the recap told them a comfortable goal was out of
// reach.
func TestDayPhrasesProduceTheRightWeek(t *testing.T) {
	cases := []struct {
		in       string
		days     string
		weekly   int
		complete bool
	}{
		{"2 hours each day, 7 days a week", "Mon,Tue,Wed,Thu,Fri,Sat,Sun", 840, true},
		{"every day for an hour", "Mon,Tue,Wed,Thu,Fri,Sat,Sun", 420, true},
		{"1 hour daily", "Mon,Tue,Wed,Thu,Fri,Sat,Sun", 420, true},
		{"weekdays, 1 hour each", "Mon,Tue,Wed,Thu,Fri", 300, true},
		{"weekends, 3 hours each", "Sat,Sun", 360, true},
		{"по 2 часа каждый день", "Mon,Tue,Wed,Thu,Fri,Sat,Sun", 840, true},
		{"har kuni 2 soat", "Mon,Tue,Wed,Thu,Fri,Sat,Sun", 840, true},
	}
	for _, c := range cases {
		av := parseAvailabilityAnswer(c.in)
		if got := strings.Join(av.Days, ","); got != c.days {
			t.Errorf("%q: days = %q, want %q", c.in, got, c.days)
		}
		if av.WeeklyMinutes != c.weekly {
			t.Errorf("%q: weeklyMinutes = %d, want %d", c.in, av.WeeklyMinutes, c.weekly)
		}
		if av.Complete() != c.complete {
			t.Errorf("%q: complete = %v, want %v", c.in, av.Complete(), c.complete)
		}
	}
}

// A day COUNT gives an exact weekly total while leaving which days to be asked.
func TestDayCountDerivesWeeklyButStillAsksWhichDays(t *testing.T) {
	av := parseAvailabilityAnswer("5 days a week, 2 hours each")
	if av.WeeklyMinutes != 600 {
		t.Errorf("weeklyMinutes = %d, want 600 (5 x 120)", av.WeeklyMinutes)
	}
	if av.HasDays() {
		t.Errorf("days = %v, want none: the count is known, the days are not", av.Days)
	}
	if missingAvailability(av) != "days" {
		t.Errorf("missing = %q, want days", missingAvailability(av))
	}
}

// A per-day duration with no idea how many days must leave the time UNKNOWN
// rather than letting a stray digit become a weekly total.
func TestPerDayDurationWithoutDaysInventsNothing(t *testing.T) {
	for _, in := range []string{"2 hours a day", "an hour a day", "по 2 часа в день"} {
		av := parseAvailabilityAnswer(in)
		if av.HasTime() {
			t.Errorf("%q: weeklyMinutes = %d, want the time left unknown until the days are known",
				in, av.WeeklyMinutes)
		}
		if av.Complete() {
			t.Errorf("%q: availability must not be complete", in)
		}
	}
	// The genuine no-unit fallback still works.
	if got := parseAvailabilityAnswer("about 6").WeeklyMinutes; got != 360 {
		t.Errorf(`"about 6" = %d minutes, want 360: a bare count is still hours per week`, got)
	}
}

// Named weekdays always beat a phrase: "1 hour each day" after listing three
// days means each of those three, not all seven.
func TestNamedWeekdaysBeatDayPhrases(t *testing.T) {
	av := parseAvailabilityAnswer("Mon, Wed and Fri, 1 hour each day")
	if got := strings.Join(av.Days, ","); got != "Mon,Wed,Fri" {
		t.Errorf("days = %q, want Mon,Wed,Fri", got)
	}
	if av.WeeklyMinutes != 180 {
		t.Errorf("weeklyMinutes = %d, want 180", av.WeeklyMinutes)
	}
}

// A deadline phrased in days must not be read as a weekly day count.
func TestDeadlineInDaysIsNotADayCount(t *testing.T) {
	for _, in := range []string{"my exam is in 5 days", "I have 30 days left"} {
		days, count := detectDayPhrases(strings.ToLower(in))
		if len(days) > 0 || count > 0 {
			t.Errorf("%q: read as %v / count %d; only \"<n> days a week\" is a day count", in, days, count)
		}
	}
}

// End to end: the capacity behind the reported bug.
//
// 14 hours a week to a deadline about 67 weeks out is roughly 900+ study hours.
// The bug reported it as 133 — a seventh of the truth — which turned a
// comfortable timeline into "insufficient".
func TestScreenshotCaseCapacityIsNotASeventh(t *testing.T) {
	loc := time.UTC
	sess := &IntakeSession{Lang: "en", Skill: "Russian", AnswerBag: map[string]string{}}
	sess.Answers.CurrentLevel = "A1"
	sess.Answers.Target = "B2"
	syncAnswersAvailability(sess, parseAvailabilityAnswer("2 hours each day, 7 days a week"))

	start := todayIn(loc).AddDate(0, 0, 1)
	sess.Answers.Deadline = dateStr(start.AddDate(0, 0, 67*7-1))

	if got := sess.Answers.Availability.WeeklyMinutes; got != 840 {
		t.Fatalf("weeklyMinutes = %d, want 840 (14h)", got)
	}
	h := planHorizon(sess, loc)
	if !h.CapacityKnown {
		t.Fatal("capacity unknown despite a complete availability")
	}
	hours := h.studyHours()
	if hours < 900 || hours > 960 {
		t.Errorf("available study hours = %d, want about 938 (67 weeks x 14h)", hours)
	}
	if hours < 200 {
		t.Fatalf("available study hours = %d — the per-day duration is being dropped again", hours)
	}
}

// The model may not supply an availability the learner never gave.
//
// Regression: asked for their level, a learner answered "i dont have
// knowledge"; the model filled in days=["Mon"], hoursPerWeek=2 alongside it.
// That completed the availability, so the interview never asked the question,
// the recap reported "2 hours per week on Mon" as though they had said it, and
// the plan was scheduled against a week they never agreed to. Removing the
// backend's own silent default was not enough while the model could invent one.
func TestModelCannotInventAnAvailability(t *testing.T) {
	claim := map[string]string{
		"currentLevel": "no knowledge",
		"days":         "Mon",
		"hoursPerWeek": "2",
	}
	for _, said := range []string{
		"i dont have knowledge",
		"work on projects",
		"By the end of 2027",
		"$50+",
	} {
		sess := &IntakeSession{Lang: "en", AnswerBag: map[string]string{}}
		// Level and target already answered, so timeBudget is what the
		// interview owes next.
		sess.Answers.CurrentLevel = "no knowledge"
		sess.Answers.Target = "work on projects"
		applyAnswers(sess, claim)
		recordStatedAvailability(sess, said, "timeBudget", claim)

		av := currentAvailability(sess)
		if av.HasDays() || av.HasTime() {
			t.Errorf("%q: recorded availability %+v that the learner never stated", said, av)
		}
		if av.Complete() {
			t.Errorf("%q: availability completed without the learner saying anything about time", said)
		}
		if nextIntakeCategory(sess) != "timeBudget" {
			t.Errorf("%q: next category = %q, want timeBudget — the question must still be asked",
				said, nextIntakeCategory(sess))
		}
	}
}

// When the learner HAS stated an availability, the model may finish a half
// their words left unreadable — but only a half that is genuinely missing.
func TestModelMayOnlyCompleteWhatTheLearnerStarted(t *testing.T) {
	// Days stated, duration phrased in a way the parser cannot structure.
	sess := &IntakeSession{Lang: "en", AnswerBag: map[string]string{}}
	recordStatedAvailability(sess, "Mondays and Thursdays, a couple of evenings",
		"timeBudget", map[string]string{"weeklyMinutes": "240"})
	av := currentAvailability(sess)
	if strings.Join(av.Days, ",") != "Mon,Thu" {
		t.Errorf("days = %v, want Mon,Thu from the learner's own words", av.Days)
	}
	if av.WeeklyMinutes != 240 {
		t.Errorf("weeklyMinutes = %d, want the model's 240 filling the unread half", av.WeeklyMinutes)
	}

	// The model must NOT override a half the learner did state.
	sess2 := &IntakeSession{Lang: "en", AnswerBag: map[string]string{}}
	recordStatedAvailability(sess2, "Tuesdays and Fridays, 1 hour each",
		"timeBudget", map[string]string{"days": "Mon", "weeklyMinutes": "600"})
	av2 := currentAvailability(sess2)
	if strings.Join(av2.Days, ",") != "Tue,Fri" {
		t.Errorf("days = %v, want the learner's Tue,Fri — not the model's Mon", av2.Days)
	}
	if av2.WeeklyMinutes != 120 {
		t.Errorf("weeklyMinutes = %d, want the learner's 120 — not the model's 600", av2.WeeklyMinutes)
	}
}

// A budget answer is not a number of hours.
func TestMoneyAnswerIsNotHours(t *testing.T) {
	for _, in := range []string{"$50+", "$50-$100", "50 USD", "around 20 dollars", "50 000 сум", "bepul"} {
		av := parseAvailabilityAnswer(in)
		if av.HasTime() {
			t.Errorf("%q: read as %d minutes a week; money is not time", in, av.WeeklyMinutes)
		}
	}
	// A real time answer still works next to a price.
	if got := parseAvailabilityAnswer("2 hours each day, 7 days a week").WeeklyMinutes; got != 840 {
		t.Errorf("weeklyMinutes = %d, want 840", got)
	}
}

// The confirmation gate must not let an approval turn revert the study week.
//
// Regression: the learner corrected their availability at the gate, the recap
// echoed 14h/week across seven days, and they approved it — then the plan was
// built as two hours on Mondays, because the model re-read an older recap out
// of the transcript and returned those figures as the "complete resulting set".
func TestApprovalCannotRevertTheStudyWeek(t *testing.T) {
	sess := &IntakeSession{Lang: "en", AnswerBag: map[string]string{}}
	sess.Answers.Target = "work on projects"
	sess.Answers.Deadline = "2027-12-31"
	syncAnswersAvailability(sess, parseAvailabilityAnswer("i can study for 2 hours every day"))

	before := currentAvailability(sess)
	if before.WeeklyMinutes != 840 || len(before.Days) != 7 {
		t.Fatalf("fixture availability = %+v, want 840 minutes across 7 days", before)
	}

	// The approval turn, with the model echoing stale figures from the transcript.
	applyFeasibilityDecision(sess, feasibilityDecision{
		Resolved: true, Approved: true,
		Target: "work on projects", Deadline: "2027-12-31",
		HoursPerWeek: 2, Days: []string{"Mon"},
	}, "Yes, build my plan")

	after := currentAvailability(sess)
	if after.WeeklyMinutes != 840 {
		t.Errorf("weeklyMinutes = %d, want 840 kept: the approval said nothing about time", after.WeeklyMinutes)
	}
	if len(after.Days) != 7 {
		t.Errorf("days = %v, want all seven kept", after.Days)
	}

	// A message that DOES state a change still applies.
	applyFeasibilityDecision(sess, feasibilityDecision{
		Resolved: true, HoursPerWeek: 6, Days: []string{"Mon", "Wed", "Fri"},
	}, "actually make it 2 hours on Monday, Wednesday and Friday")
	changed := currentAvailability(sess)
	if strings.Join(changed.Days, ",") != "Mon,Wed,Fri" {
		t.Errorf("days = %v, want Mon,Wed,Fri", changed.Days)
	}
	if changed.WeeklyMinutes != 360 {
		t.Errorf("weeklyMinutes = %d, want 360 from the learner's own words", changed.WeeklyMinutes)
	}
}

// TEST 9 — a deadline on its own is not availability.
func TestDeadlineAloneLeavesAvailabilityUnknown(t *testing.T) {
	av := availabilityFrom(t, "I want IELTS 7+ by December 31")
	if av.HasDays() || av.HasTime() {
		t.Fatalf("a deadline produced an availability: %+v", av)
	}
	if av.Complete() {
		t.Fatal("a deadline must never complete the availability")
	}

	// And the pipeline must ask for it rather than assume one.
	sess := &IntakeSession{Lang: "en", Skill: "IELTS", AnswerBag: map[string]string{}}
	sess.Answers.CurrentLevel = "band 6"
	sess.Answers.Target = "band 7"
	sess.Answers.Deadline = "2026-12-31"

	if nextIntakeCategory(sess) != "timeBudget" {
		t.Errorf("next question = %q, want timeBudget: availability is still missing", nextIntakeCategory(sess))
	}
	// Capacity must report itself as unknown, not as a number.
	h := planHorizon(sess, time.UTC)
	if h.CapacityKnown {
		t.Error("capacity claims to be known with no availability")
	}
	if h.studyHours() != -1 {
		t.Errorf("studyHours = %d, want -1 (unknown), never a derived figure", h.studyHours())
	}
	if !h.Assumed {
		t.Error("the scheduler fallback must be flagged as assumed")
	}
}

// TEST 10 — a weekly figure with no days still needs the days.
func TestWeeklyTimeWithoutDaysAsksForDays(t *testing.T) {
	av := availabilityFrom(t, "I can study 5 hours per week")
	if av.WeeklyMinutes != 300 {
		t.Errorf("weeklyMinutes = %d, want 300", av.WeeklyMinutes)
	}
	if av.HasDays() {
		t.Errorf("days = %v, want none", av.Days)
	}
	if missingAvailability(av) != "days" {
		t.Errorf("missing = %q, want days", missingAvailability(av))
	}

	sess := &IntakeSession{Lang: "en", AnswerBag: map[string]string{}}
	sess.Answers.CurrentLevel, sess.Answers.Target, sess.Answers.Deadline = "a", "b", "2026-12-31"
	syncAnswersAvailability(sess, av)
	if nextIntakeCategory(sess) != "timeBudget" {
		t.Error("with days missing, the interview must come back for them")
	}
	if !strings.Contains(timeBudgetMeaning(sess), "days") {
		t.Errorf("the follow-up asks the wrong thing: %q", timeBudgetMeaning(sess))
	}
	if strings.Contains(strings.ToLower(timeBudgetMeaning(sess)), "how much time they can study on those days") {
		t.Error("it must not re-ask for the time it already has")
	}
}

// Days with no duration ask only for the duration.
func TestDaysWithoutTimeAsksForTime(t *testing.T) {
	av := availabilityFrom(t, "Monday and Saturday")
	if !av.HasDays() || av.HasTime() {
		t.Fatalf("parsed = %+v, want days only", av)
	}
	if missingAvailability(av) != "time" {
		t.Errorf("missing = %q, want time", missingAvailability(av))
	}
	sess := &IntakeSession{Lang: "en", AnswerBag: map[string]string{}}
	syncAnswersAvailability(sess, av)
	m := timeBudgetMeaning(sess)
	if !strings.Contains(m, "how much time") {
		t.Errorf("follow-up = %q, want it to ask for the duration", m)
	}
	if !strings.Contains(m, "do NOT ask which days again") {
		t.Error("the follow-up must forbid re-asking for the days")
	}
}

// The no-key interview must not invent an availability either, and must come
// back for it exactly once before giving up.
func TestMockInterviewAsksAgainRatherThanDefaulting(t *testing.T) {
	sess := &IntakeSession{Lang: "en", Skill: "Chess", AnswerBag: map[string]string{}}
	ingestAnswer(sess, "beginner")
	ingestAnswer(sess, "club level")
	ingestAnswer(sess, "no deadline")

	ingestAnswer(sess, "not sure really") // says nothing about availability
	if sess.Answers.Availability.Complete() {
		t.Fatal("an answer with no availability in it completed the availability")
	}
	if sess.Answers.HoursPerWeek != 0 {
		t.Errorf("HoursPerWeek = %d, want 0: no default may be invented", sess.Answers.HoursPerWeek)
	}
	if answered(sess, "time") {
		t.Error("the interview gave up after one attempt")
	}

	ingestAnswer(sess, "Tuesdays and Thursdays, 45 minutes each")
	if !sess.Answers.Availability.Complete() {
		t.Fatalf("availability still incomplete: %+v", sess.Answers.Availability)
	}
	if sess.Answers.Availability.WeeklyMinutes != 90 {
		t.Errorf("weeklyMinutes = %d, want 90", sess.Answers.Availability.WeeklyMinutes)
	}
}

// Merging must never blank out the half it was not told about.
func TestMergeAvailabilityKeepsTheOtherHalf(t *testing.T) {
	base := StudyAvailability{Days: []string{"Mon", "Wed", "Fri"}, WeeklyMinutes: 360}
	base.normalize()

	onlyDays := mergeAvailability(base, StudyAvailability{Days: []string{"Fri", "Sat"}})
	if onlyDays.WeeklyMinutes != 360 {
		t.Errorf("weeklyMinutes = %d, want the previous 360 kept", onlyDays.WeeklyMinutes)
	}
	if strings.Join(onlyDays.Days, ",") != "Fri,Sat" {
		t.Errorf("days = %v, want Fri,Sat", onlyDays.Days)
	}

	onlyTime := mergeAvailability(base, StudyAvailability{WeeklyMinutes: 180})
	if strings.Join(onlyTime.Days, ",") != "Mon,Wed,Fri" {
		t.Errorf("days = %v, want them untouched", onlyTime.Days)
	}
	if onlyTime.WeeklyMinutes != 180 {
		t.Errorf("weeklyMinutes = %d, want 180", onlyTime.WeeklyMinutes)
	}
}

// Availability figures are bounded exactly as typed intake answers are.
func TestAvailabilityIsClamped(t *testing.T) {
	av := StudyAvailability{Days: []string{"Mon"}, WeeklyMinutes: 99999}
	av.normalize()
	if av.WeeklyMinutes != maxWeeklyMinutes {
		t.Errorf("weeklyMinutes = %d, want it clamped to %d", av.WeeklyMinutes, maxWeeklyMinutes)
	}
	perDay := StudyAvailability{PerDay: []DayAvailability{{Weekday: "Mon", Minutes: 5000}}}
	perDay.normalize()
	if perDay.PerDay[0].Minutes != maxDayMinutes {
		t.Errorf("per-day = %d, want it clamped to %d", perDay.PerDay[0].Minutes, maxDayMinutes)
	}
}

// ---- capacity ----

// TEST 11 — capacity is the learner's own schedulable time, not calendar time.
func TestCapacityUsesRealAvailabilityNotCalendarTime(t *testing.T) {
	av := StudyAvailability{Days: []string{"Mon", "Wed", "Fri"}, WeeklyMinutes: 180}
	av.normalize()

	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) // a Monday
	end := start.AddDate(0, 0, 10*7-1)                   // exactly ten weeks

	mins, known := availableStudyMinutes(av, start, end)
	if !known {
		t.Fatal("capacity should be known: availability is complete")
	}
	hours := mins / 60
	if hours != 30 {
		t.Errorf("available study hours = %d, want about 30 (10 weeks x 3h)", hours)
	}
	// The failure this exists to prevent.
	if hours > 100 {
		t.Fatalf("capacity = %d hours — calendar time was counted as study time", hours)
	}
	calendarHours := int(end.Sub(start).Hours())
	if hours >= calendarHours/10 {
		t.Errorf("capacity %dh is implausibly close to the %dh of calendar time in the window", hours, calendarHours)
	}
}

// An incomplete availability yields no capacity at all, not zero.
func TestCapacityIsUnknownWithoutAvailability(t *testing.T) {
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 70)

	for _, av := range []StudyAvailability{
		{},
		{Days: []string{"Mon"}},
		{WeeklyMinutes: 180},
	} {
		if _, known := availableStudyMinutes(av, start, end); known {
			t.Errorf("capacity reported as known for %+v", av)
		}
	}
}

// Per-day limits are respected when counting capacity.
func TestCapacityHonoursPerDayLimits(t *testing.T) {
	av := StudyAvailability{PerDay: []DayAvailability{
		{Weekday: "Mon", Minutes: 120},
		{Weekday: "Sat", Minutes: 180},
	}}
	av.normalize()
	if av.WeeklyMinutes != 300 {
		t.Fatalf("weeklyMinutes = %d, want 300", av.WeeklyMinutes)
	}
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) // Monday
	end := start.AddDate(0, 0, 4*7-1)                    // four weeks
	mins, known := availableStudyMinutes(av, start, end)
	if !known {
		t.Fatal("capacity should be known")
	}
	if mins != 4*300 {
		t.Errorf("capacity = %d minutes, want %d", mins, 4*300)
	}
}

// A deadline already gone by yields no capacity, and does not go negative.
func TestCapacityForAPastDeadlineIsZero(t *testing.T) {
	av := StudyAvailability{Days: []string{"Mon"}, WeeklyMinutes: 60}
	av.normalize()
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mins, known := availableStudyMinutes(av, start, start.AddDate(0, 0, -30))
	if !known || mins != 0 {
		t.Errorf("got (%d, %v), want (0, true)", mins, known)
	}
}
