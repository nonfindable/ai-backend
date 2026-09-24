package main

import (
	"strings"
	"testing"
	"time"
)

// ---- feasibility ----
//
// Two things are being defended here. Capacity comes from the learner's real
// availability, never from calendar time. And the verdict never makes a claim
// the arithmetic cannot support: "there is not enough scheduled time for that"
// is supportable; "you cannot reach IELTS 7" is not.

// buildSession assembles a session with a known availability and deadline.
func buildSession(t *testing.T, deadline string, days []string, weeklyMinutes int) *IntakeSession {
	t.Helper()
	sess := &IntakeSession{Lang: "en", Skill: "IELTS", AnswerBag: map[string]string{}}
	sess.Answers.CurrentLevel = "band 5.5"
	sess.Answers.Target = "band 7.0"
	sess.Answers.Deadline = deadline
	if len(days) > 0 || weeklyMinutes > 0 {
		av := StudyAvailability{Days: days, WeeklyMinutes: weeklyMinutes}
		av.normalize()
		syncAnswersAvailability(sess, av)
	}
	return sess
}

// deadlineWeeksOut returns a YYYY-MM-DD n weeks after tomorrow, so the horizon
// arithmetic is exact regardless of when the test runs.
func deadlineWeeksOut(loc *time.Location, weeks int) string {
	start := todayIn(loc).AddDate(0, 0, 1)
	return dateStr(start.AddDate(0, 0, weeks*7-1))
}

// TEST 11 — ten weeks at three hours a week is about thirty study hours.
func TestFeasibilityCapacityIsRealStudyTime(t *testing.T) {
	loc := time.UTC
	sess := buildSession(t, deadlineWeeksOut(loc, 10), []string{"Mon", "Wed", "Fri"}, 180)

	h := planHorizon(sess, loc)
	if !h.CapacityKnown {
		t.Fatal("capacity should be known")
	}
	if h.Assumed {
		t.Fatal("availability was stated, so nothing should be assumed")
	}
	got := h.studyHours()
	if got < 28 || got > 32 {
		t.Errorf("available study hours = %d, want about 30", got)
	}
	// The bug this exists to catch: 10 weeks of calendar hours is 1680.
	if got > 200 {
		t.Fatalf("available study hours = %d — calendar time is being counted as study time", got)
	}
	if h.WeeksUntilDeadline != 10 {
		t.Errorf("weeksUntilDeadline = %d, want 10", h.WeeksUntilDeadline)
	}
}

// The capacity handed to the model is the backend's, and it is not the
// deadline's span.
func TestFeasibilityPayloadCarriesMeasuredCapacity(t *testing.T) {
	loc := time.UTC
	store := newStore(t.TempDir())
	t.Cleanup(store.Close)
	cfg := Config{DataDir: t.TempDir(), DefaultTimezone: "UTC"}
	gw := newGateway(cfg)
	t.Cleanup(gw.Close)
	p := newPipeline(cfg, store, gw, newScheduler(store))

	sess := buildSession(t, deadlineWeeksOut(loc, 10), []string{"Mon", "Wed", "Fri"}, 180)
	sess.UserID = "u1"
	store.SaveUser(&User{ID: "u1", Timezone: "UTC"})

	payload := p.buildFeasibilityContext(sess, loc)
	if !strings.Contains(payload, `"capacityKnown":true`) {
		t.Errorf("payload does not report capacity as known:\n%s", payload)
	}
	if !strings.Contains(payload, `"availableStudyHours":30`) {
		t.Errorf("payload should carry 30 measured study hours:\n%s", payload)
	}
	if strings.Contains(payload, "totalHoursAvailable") {
		t.Error("the old deadline-derived field is still being sent")
	}
	if !strings.Contains(payload, `"weeklyMinutes":180`) {
		t.Error("the weekly minute budget was not sent")
	}
}

// With availability unknown, the payload says so instead of sending a number.
func TestFeasibilityPayloadReportsUnknownCapacity(t *testing.T) {
	loc := time.UTC
	store := newStore(t.TempDir())
	t.Cleanup(store.Close)
	cfg := Config{DataDir: t.TempDir(), DefaultTimezone: "UTC"}
	gw := newGateway(cfg)
	t.Cleanup(gw.Close)
	p := newPipeline(cfg, store, gw, newScheduler(store))

	sess := buildSession(t, deadlineWeeksOut(loc, 10), nil, 0)
	sess.UserID = "u1"
	store.SaveUser(&User{ID: "u1", Timezone: "UTC"})

	payload := p.buildFeasibilityContext(sess, loc)
	if !strings.Contains(payload, `"capacityKnown":false`) {
		t.Errorf("payload claims capacity is known with no availability:\n%s", payload)
	}
	if !strings.Contains(payload, `"availableStudyHours":-1`) {
		t.Errorf("unknown capacity must be -1, never a derived figure:\n%s", payload)
	}
}

// TEST 12 — the verdict never makes an absolute claim.
//
// The wording tested is the backend's own (the mock gate), because a live
// model's prose is not a contract. What IS pinned for live mode is the bounded
// status enum, which has no value meaning "impossible".
func TestFeasibilityWordingIsNeverAbsolute(t *testing.T) {
	loc := time.UTC
	// A deliberately under-resourced goal: 30 hours for 5.5 -> 7.0.
	sess := buildSession(t, deadlineWeeksOut(loc, 10), []string{"Mon", "Wed", "Fri"}, 180)
	h := planHorizon(sess, loc)
	r := mockConfirm(sess, h, "")

	text := strings.ToLower(r.Summary + " " + r.Verdict + " " + r.Question)
	forbidden := []string{
		"impossible", "you cannot", "you can't", "will never", "cannot reach",
		"not possible", "no chance", "unachievable",
	}
	for _, f := range forbidden {
		if strings.Contains(text, f) {
			t.Errorf("verdict makes an unsupported absolute claim (%q): %s", f, r.Verdict)
		}
	}
	// And it must not overclaim in the other direction either.
	for _, f := range []string{"guaranteed", "certainly reach", "definitely reach"} {
		if strings.Contains(text, f) {
			t.Errorf("verdict overpromises (%q): %s", f, r.Verdict)
		}
	}
	if !strings.Contains(r.Verdict, "30") {
		t.Errorf("verdict does not state the measured capacity: %s", r.Verdict)
	}
}

// The status enum is bounded and has no "impossible" value.
func TestFeasibilityStatusIsBounded(t *testing.T) {
	for _, s := range []string{feasibleStatus, tightStatus, insufficientStatus, unknownStatus} {
		if normalizeFeasibilityStatus(s) != s {
			t.Errorf("%q did not round-trip", s)
		}
	}
	for _, s := range []string{"impossible", "", "yes", "no", "REACHABLE", "true"} {
		if got := normalizeFeasibilityStatus(s); got != unknownStatus {
			t.Errorf("normalizeFeasibilityStatus(%q) = %q, want %q", s, got, unknownStatus)
		}
	}
}

// With availability unknown, the mock gate declines to assess rather than
// assessing against a default.
func TestConfirmWithUnknownAvailabilitySaysSo(t *testing.T) {
	loc := time.UTC
	sess := buildSession(t, deadlineWeeksOut(loc, 10), nil, 0)
	h := planHorizon(sess, loc)
	r := mockConfirm(sess, h, "")

	if r.Status != unknownStatus {
		t.Errorf("status = %q, want %q", r.Status, unknownStatus)
	}
	if r.AvailableHours != -1 {
		t.Errorf("availableHours = %d, want -1", r.AvailableHours)
	}
	low := strings.ToLower(r.Verdict)
	if !strings.Contains(low, "availability") {
		t.Errorf("verdict should name the missing availability: %s", r.Verdict)
	}
}

// The gate must send the interview back for a missing availability rather than
// presenting arithmetic built on a default.
func TestGateAsksForAvailabilityBeforeConfirming(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	sess := newSession(t, c)

	// Answer everything EXCEPT availability, and never state one.
	script := []string{
		"I want IELTS 7.0", "Academic", "band 5.5", "band 7.0", "2026-12-01",
	}
	var turn Turn
	for _, m := range script {
		turn = chatTurn(t, c, sess, m)
	}
	if turn.Stage != "intake" {
		t.Fatalf("stage = %q, want intake (availability is still unknown)", turn.Stage)
	}
	// The pending question must be about availability, not the budget.
	low := strings.ToLower(turn.Question)
	if !strings.Contains(low, "day") && !strings.Contains(low, "time") {
		t.Errorf("question = %q, want it to ask for study availability", turn.Question)
	}

	stored, _ := ts.store.GetSession(sess)
	if stored.Answers.Availability.Complete() {
		t.Error("availability was completed without the user stating one")
	}
	if stored.FeasibilityAgreed {
		t.Error("nothing may be agreed before availability is known")
	}
}

// The reported Golang transcript, end to end.
//
// The learner answered level, target, deadline and budget, and was never asked
// when they could study — yet the recap claimed "2 hours per week on Mon" and
// declared the goal out of reach on 132 hours. The interview must not reach the
// gate until it has actually asked, and the answer it gets must be the one the
// plan is built from.
func TestInterviewCannotReachTheGateWithoutAskingAboutTime(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	sess := newSession(t, c)

	var turn Turn
	for _, msg := range []string{"Golang", "i dont have knowledge", "work on projects", "By the end of 2027", "$50+"} {
		turn = chatTurn(t, c, sess, msg)
		stored, _ := ts.store.GetSession(sess)
		if av := currentAvailability(stored); av.Complete() {
			t.Fatalf("after %q the availability was already complete (%+v) — nobody asked", msg, av)
		}
	}

	if turn.Stage == "confirm_plan" {
		t.Fatalf("reached the confirmation gate without an availability: %q", turn.Assistant)
	}
	if turn.Stage != "intake" {
		t.Fatalf("stage = %q, want intake", turn.Stage)
	}
	low := strings.ToLower(turn.Question)
	if !strings.Contains(low, "day") && !strings.Contains(low, "time") {
		t.Errorf("pending question = %q, want it to ask when they can study", turn.Question)
	}

	// Answering it produces the real week, and the recap is built from that.
	turn = chatTurn(t, c, sess, "i can study for 2 hours every day")
	stored, _ := ts.store.GetSession(sess)
	av := currentAvailability(stored)
	if av.WeeklyMinutes != 840 {
		t.Errorf("weeklyMinutes = %d, want 840 (2h x 7 days)", av.WeeklyMinutes)
	}
	if len(av.Days) != 7 {
		t.Errorf("days = %v, want all seven", av.Days)
	}
	// Answer whatever else the interview still wants, then approve. The point
	// is that the availability recorded above is what survives to the plan.
	for i := 0; turn.Stage == "intake" && i < maxIntakeQuestions; i++ {
		turn = chatTurn(t, c, sess, "free materials")
	}
	if turn.Stage != "confirm_plan" {
		t.Fatalf("stage = %q, want confirm_plan once availability is known", turn.Stage)
	}
	if av := currentAvailability(mustSession(t, ts, sess)); av.WeeklyMinutes != 840 || len(av.Days) != 7 {
		t.Fatalf("availability drifted before the gate: %+v", av)
	}

	// Approving must build the plan for THAT week, not an earlier one.
	turn = chatTurn(t, c, sess, "yes, build my plan")
	if turn.PlanID == "" {
		t.Fatalf("no plan was built: stage %q", turn.Stage)
	}
	plan, ok := ts.store.GetPlan(turn.PlanID)
	if !ok {
		t.Fatal("plan missing")
	}
	if plan.WeeklyMinutes != 840 {
		t.Errorf("plan weeklyMinutes = %d, want 840", plan.WeeklyMinutes)
	}
	if len(plan.Days) != 7 {
		t.Errorf("plan days = %v, want all seven — not the week nobody agreed to", plan.Days)
	}

	// And the calendar uses the whole week rather than piling onto one day.
	var out map[string]any
	if code := c.do("POST", "/api/schedule", map[string]any{"planId": turn.PlanID}, &out); code != 200 {
		t.Fatalf("schedule: %d", code)
	}
	used := map[string]bool{}
	for _, ev := range ts.store.EventsForPlan(turn.PlanID) {
		if d, ok := parseDateIn(ev.Date, time.UTC); ok {
			used[weekdayCode(d.Weekday())] = true
		}
	}
	if len(used) < 5 {
		t.Errorf("sessions landed on only %d distinct weekdays (%v); 14h/week across 7 days should spread out", len(used), used)
	}
}

func mustSession(t *testing.T, ts *testServer, id string) *IntakeSession {
	t.Helper()
	s, ok := ts.store.GetSession(id)
	if !ok {
		t.Fatal("session missing")
	}
	return s
}

// A plan carries the measured capacity into its context, and the plan prompt
// never receives calendar hours.
func TestPlanContextCarriesMeasuredCapacity(t *testing.T) {
	loc := time.UTC
	store := newStore(t.TempDir())
	t.Cleanup(store.Close)
	cfg := Config{DataDir: t.TempDir(), DefaultTimezone: "UTC"}
	gw := newGateway(cfg)
	t.Cleanup(gw.Close)
	p := newPipeline(cfg, store, gw, newScheduler(store))

	sess := buildSession(t, deadlineWeeksOut(loc, 10), []string{"Mon", "Wed", "Fri"}, 180)
	sess.UserID = "u1"
	store.SaveUser(&User{ID: "u1", Timezone: "UTC"})

	payload := p.buildPlanContext(sess, loc)
	if strings.Contains(payload, "totalHoursAvailable") {
		t.Error("the plan prompt still receives the deadline-derived capacity")
	}
	if !strings.Contains(payload, `"availableStudyHours":30`) {
		t.Errorf("plan context should carry the measured 30 hours:\n%s", payload)
	}
	if !strings.Contains(payload, `"weeklyMinuteBudget":180`) {
		t.Errorf("plan context should carry the exact minute budget:\n%s", payload)
	}
}
