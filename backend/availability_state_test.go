package main

import (
	"strings"
	"testing"
	"time"
)

// ---- per-day vs per-week, and derived state ----
//
// The reported regression in one line: "everyday 2hours" became two hours a
// week. Three separate defects fed it, and each has a test here.

// TEST 17 — parsing, with and without a week already in force.
func TestPerDayRateResolvesAgainstKnownDays(t *testing.T) {
	sevenDays := StudyAvailability{Days: allWeekdays, WeeklyMinutes: 840}
	sevenDays.normalize()
	mwf := StudyAvailability{Days: []string{"Mon", "Wed", "Fri"}, WeeklyMinutes: 360}
	mwf.normalize()

	cases := []struct {
		in      string
		current StudyAvailability
		weekly  int
		days    string
	}{
		// A rate, with the week stated in the same breath.
		{"everyday 2hours", StudyAvailability{}, 840, "Mon,Tue,Wed,Thu,Fri,Sat,Sun"},
		{"every day for 2 hours", StudyAvailability{}, 840, "Mon,Tue,Wed,Thu,Fri,Sat,Sun"},
		// A rate, with the week already known from earlier in the conversation.
		{"2 hours per day", sevenDays, 840, "Mon,Tue,Wed,Thu,Fri,Sat,Sun"},
		{"5h/day", sevenDays, 2100, "Mon,Tue,Wed,Thu,Fri,Sat,Sun"},
		{"2 hours per day", mwf, 360, "Mon,Wed,Fri"},
		// A rate and a week in one message.
		{"Monday Wednesday Friday, 2h/day", StudyAvailability{}, 360, "Mon,Wed,Fri"},
		// Totals stay totals.
		{"5h/week", StudyAvailability{}, 300, ""},
		{"35 hours per week", StudyAvailability{}, 2100, ""},
		{"5h/week", sevenDays, 300, "Mon,Tue,Wed,Thu,Fri,Sat,Sun"},
		// A counted week without named days.
		{"about 1 hour on three days", StudyAvailability{}, 180, ""},
	}
	for _, c := range cases {
		got := resolveAvailability(c.current, parseAvailabilityStatement(c.in), true)
		if got.WeeklyMinutes != c.weekly {
			t.Errorf("%q (current %dm/%v): weeklyMinutes = %d, want %d",
				c.in, c.current.WeeklyMinutes, c.current.Days, got.WeeklyMinutes, c.weekly)
		}
		if d := strings.Join(got.Days, ","); d != c.days {
			t.Errorf("%q: days = %q, want %q", c.in, d, c.days)
		}
	}
}

// A per-day rate and a per-week total must never collapse into one integer.
func TestPerDayAndPerWeekAreNotTheSameNumber(t *testing.T) {
	perDay := parseAvailabilityStatement("2 hours per day")
	perWeek := parseAvailabilityStatement("2 hours per week")

	if perDay.UniformDay != 120 {
		t.Errorf("per-day rate = %d, want 120", perDay.UniformDay)
	}
	if perDay.Weekly != 0 {
		t.Errorf("per-day statement set a weekly total of %d", perDay.Weekly)
	}
	if perWeek.Weekly != 120 {
		t.Errorf("per-week total = %d, want 120", perWeek.Weekly)
	}
	if perWeek.UniformDay != 0 {
		t.Errorf("per-week statement set a per-day rate of %d", perWeek.UniformDay)
	}
	for _, in := range []string{"2 hours every day", "everyday 2 hours", "2h/day"} {
		if st := parseAvailabilityStatement(in); st.UniformDay != 120 {
			t.Errorf("%q: per-day rate = %d, want 120", in, st.UniformDay)
		}
	}
	for _, in := range []string{"2 hours per week", "2h weekly", "two hours a week"} {
		if st := parseAvailabilityStatement(in); st.Weekly != 120 {
			t.Errorf("%q: weekly total = %d, want 120", in, st.Weekly)
		}
	}
}

// TEST 5 — the latest explicit correction replaces the previous value.
func TestLatestCorrectionReplacesTheWeek(t *testing.T) {
	sess := &IntakeSession{Lang: "en", AnswerBag: map[string]string{}}

	recordStatedAvailability(sess, "everyday 2hours", "timeBudget", nil)
	if got := currentAvailability(sess).WeeklyMinutes; got != 840 {
		t.Fatalf("after 'everyday 2hours' weeklyMinutes = %d, want 840", got)
	}

	recordStatedAvailability(sess, "actually make it 5 hours per day", "", nil)
	av := currentAvailability(sess)
	if av.WeeklyMinutes != 2100 {
		t.Errorf("weeklyMinutes = %d, want 2100 (5h x 7 days)", av.WeeklyMinutes)
	}
	for _, pd := range av.PerDay {
		if pd.Minutes != 300 {
			t.Errorf("%s = %d minutes, want 300", pd.Weekday, pd.Minutes)
		}
	}

	// A weekly total replaces the rate, and does not pretend to know the split.
	recordStatedAvailability(sess, "actually 7 hours per week", "", nil)
	av = currentAvailability(sess)
	if av.WeeklyMinutes != 420 {
		t.Errorf("weeklyMinutes = %d, want 420", av.WeeklyMinutes)
	}
	if len(av.PerDay) != 0 {
		t.Errorf("per-day detail = %+v, want it dropped: the split is unknown again", av.PerDay)
	}
	if len(av.Days) != 7 {
		t.Errorf("days = %v, want the seven-day week kept", av.Days)
	}
}

// TEST 16 — no deadline means no fabricated capacity and no invented year.
func TestNoDeadlineMeansNoCapacityFigure(t *testing.T) {
	sess := &IntakeSession{Lang: "en", Skill: "IELTS", AnswerBag: map[string]string{}}
	sess.Answers.CurrentLevel = "band 6"
	sess.Answers.Target = "band 7+"
	recordStatedAvailability(sess, "everyday 2hours", "timeBudget", nil)

	if sess.Answers.Deadline != "" {
		t.Fatalf("a deadline appeared from nowhere: %q", sess.Answers.Deadline)
	}
	h := planHorizon(sess, time.UTC)
	if h.HasDeadline {
		t.Error("horizon claims a deadline the learner never gave")
	}
	if h.CapacityKnown {
		t.Errorf("capacity reported as known with no deadline: %d hours", h.studyHours())
	}
	if h.studyHours() != -1 {
		t.Errorf("studyHours = %d, want -1; a capacity 'before the deadline' needs a deadline", h.studyHours())
	}
	// The exact fabricated value from the report: 52 weeks x 2h.
	if h.studyHours() == 103 || h.studyHours() == 104 {
		t.Fatal("the hidden 52-week horizon is back")
	}
	if h.WeeksUntilDeadline != 0 {
		t.Errorf("weeksUntilDeadline = %d, want 0", h.WeeksUntilDeadline)
	}
}

// Without a deadline the gate offers a PROJECTED FINISH, clearly labelled.
func TestNoDeadlineProducesAProjectedFinishNotADeadline(t *testing.T) {
	sess := &IntakeSession{Lang: "en", Skill: "IELTS", AnswerBag: map[string]string{}}
	sess.Answers.CurrentLevel = "band 6"
	sess.Answers.Target = "band 7+"
	recordStatedAvailability(sess, "everyday 2hours", "timeBudget", nil) // 14h/week

	h := planHorizon(sess, time.UTC)
	// A 140-210 hour estimate at 14h/week is 10-15 weeks.
	if got := projectedWeeks(140, h.Availability); got != 10 {
		t.Errorf("projectedWeeks(140) = %d, want 10", got)
	}
	if got := projectedWeeks(210, h.Availability); got != 15 {
		t.Errorf("projectedWeeks(210) = %d, want 15", got)
	}

	line := projectionLine("en", h, 140, 210)
	if line == "" {
		t.Fatal("no projection offered despite a known availability")
	}
	low := strings.ToLower(line)
	if !strings.Contains(low, "projected finish") {
		t.Errorf("projection is not labelled as one: %q", line)
	}
	if !strings.Contains(low, "not a date you've committed to") {
		t.Errorf("projection does not disclaim being a deadline: %q", line)
	}
	if strings.Contains(low, "before your deadline") {
		t.Errorf("projection refers to a deadline that does not exist: %q", line)
	}

	// With a deadline, there is no projection — the capacity figure applies.
	sess.Answers.Deadline = dateStr(todayIn(time.UTC).AddDate(0, 0, 200))
	if got := projectionLine("en", planHorizon(sess, time.UTC), 140, 210); got != "" {
		t.Errorf("projection offered alongside a real deadline: %q", got)
	}
}

// TEST 15 — derived values are invalidated when availability changes.
func TestChangingAvailabilityInvalidatesDerivedState(t *testing.T) {
	loc := time.UTC
	sess := &IntakeSession{Lang: "en", Skill: "IELTS", AnswerBag: map[string]string{}}
	sess.Answers.CurrentLevel = "band 6"
	sess.Answers.Target = "band 7+"
	sess.Answers.Deadline = dateStr(todayIn(loc).AddDate(0, 0, 1).AddDate(0, 0, 10*7-1)) // 10 weeks

	recordStatedAvailability(sess, "2 hours per week on Mon", "timeBudget", nil)
	first := planHorizon(sess, loc).studyHours()
	if first <= 0 {
		t.Fatalf("no capacity computed: %d", first)
	}

	// Pretend a verdict was reached and agreed against that week.
	sess.FeasibilityNote = "stale verdict"
	sess.FeasibilityStatus = insufficientStatus
	sess.FeasibilityAgreed = true
	stale := answersFingerprint(sess)

	recordStatedAvailability(sess, "5 hours per week", "timeBudget", nil)

	if sess.FeasibilityNote != "" || sess.FeasibilityStatus != "" {
		t.Errorf("a verdict computed from the old week survived: %q / %q",
			sess.FeasibilityNote, sess.FeasibilityStatus)
	}
	if sess.FeasibilityAgreed {
		t.Error("an approval of the old week survived the change")
	}
	if answersFingerprint(sess) == stale {
		t.Error("the recap fingerprint did not change, so no fresh recap would be produced")
	}

	second := planHorizon(sess, loc).studyHours()
	if second == first {
		t.Errorf("capacity is still %d after changing 2h/week to 5h/week", second)
	}
	if second < first {
		t.Errorf("capacity fell from %d to %d when the week grew", first, second)
	}

	// And again, at a much larger figure. Note the week widens too: 5h/day on
	// a single available day is still only 5h a week, which is correct and is
	// why the day set has to move with it.
	recordStatedAvailability(sess, "every day, 5 hours each", "timeBudget", nil)
	third := planHorizon(sess, loc).studyHours()
	if got := currentAvailability(sess).WeeklyMinutes; got != 2100 {
		t.Fatalf("weeklyMinutes = %d, want 2100", got)
	}
	if third <= second {
		t.Errorf("capacity = %d, want more than %d after moving to 5h/day across the week", third, second)
	}
}

// A per-day change that leaves the rounded hours alone must still invalidate.
func TestFingerprintNoticesADayCountChange(t *testing.T) {
	sess := &IntakeSession{Lang: "en", AnswerBag: map[string]string{}}
	recordStatedAvailability(sess, "2 hours each day, 7 days a week", "timeBudget", nil)
	before := answersFingerprint(sess)

	recordStatedAvailability(sess, "Monday, Wednesday and Friday, 2 hours each", "timeBudget", nil)
	if answersFingerprint(sess) == before {
		t.Error("halving the week did not change the recap fingerprint")
	}
	if got := currentAvailability(sess).WeeklyMinutes; got != 360 {
		t.Errorf("weeklyMinutes = %d, want 360", got)
	}
}

// TEST 14 — the reported conversation, step by step.
func TestReportedAvailabilityConversation(t *testing.T) {
	sess := &IntakeSession{Lang: "en", Skill: "IELTS", AnswerBag: map[string]string{}}
	sess.Answers.Target = "7+"
	sess.Answers.CurrentLevel = "6 (l6, r6, w5.5, s6)"

	// "everyday 2hours" -> seven days, 120 minutes each, 840 a week.
	recordStatedAvailability(sess, "everyday 2hours", "timeBudget", nil)
	av := currentAvailability(sess)
	if len(av.Days) != 7 {
		t.Fatalf("days = %v, want all seven", av.Days)
	}
	if av.WeeklyMinutes != 840 {
		t.Fatalf("weeklyMinutes = %d, want 840 (14 hours), NOT 120", av.WeeklyMinutes)
	}
	for _, pd := range av.PerDay {
		if pd.Minutes != 120 {
			t.Errorf("%s = %d minutes, want 120", pd.Weekday, pd.Minutes)
		}
	}
	// No deadline was given, so there is no total available before one.
	if h := planHorizon(sess, time.UTC); h.CapacityKnown || h.studyHours() != -1 {
		t.Errorf("capacity %d reported with no deadline", h.studyHours())
	}

	// "2 hours per day" -> unchanged, still 14 hours a week. Never 2.
	recordStatedAvailability(sess, "2 hours per day", "", nil)
	if got := currentAvailability(sess).WeeklyMinutes; got != 840 {
		t.Errorf("weeklyMinutes = %d, want 840 held; a per-day restatement is not 2h/week", got)
	}

	// "5h/day" -> 300 minutes each, 2100 a week.
	recordStatedAvailability(sess, "5h/day", "", nil)
	av = currentAvailability(sess)
	if av.WeeklyMinutes != 2100 {
		t.Errorf("weeklyMinutes = %d, want 2100 (35 hours)", av.WeeklyMinutes)
	}
	for _, pd := range av.PerDay {
		if pd.Minutes != 300 {
			t.Errorf("%s = %d minutes, want 300", pd.Weekday, pd.Minutes)
		}
	}

	// Selecting the option chip runs the same mutation path as typing it.
	applyFeasibilityDecision(sess, feasibilityDecision{Resolved: true, HoursPerWeek: 5},
		"Keep target 7+, study 5 hours per week")
	av = currentAvailability(sess)
	if av.WeeklyMinutes != 300 {
		t.Errorf("weeklyMinutes = %d, want 300; the 35-hour week must be replaced", av.WeeklyMinutes)
	}
	if len(av.PerDay) != 0 {
		t.Errorf("per-day = %+v, want none: 5 hours a week is not 5 hours a day", av.PerDay)
	}

	// "extend the deadline" when none was ever set must not resurrect anything.
	before := availabilityFingerprint(currentAvailability(sess))
	applyFeasibilityDecision(sess, feasibilityDecision{Resolved: true, HoursPerWeek: 2, Days: []string{"Mon"}},
		"Keep target 7+ and extend deadline to fit needed hours")
	after := currentAvailability(sess)
	if availabilityFingerprint(after) != before {
		t.Errorf("availability changed on a deadline-only message: %+v", after)
	}
	if after.WeeklyMinutes == 120 {
		t.Error("the old 2h/week was restored")
	}
	if sess.Answers.Deadline != "" {
		t.Errorf("a deadline was invented: %q", sess.Answers.Deadline)
	}
	if h := planHorizon(sess, time.UTC); h.studyHours() == 103 || h.studyHours() == 104 {
		t.Errorf("the fabricated 103-hour figure is back: %d", h.studyHours())
	}
}
