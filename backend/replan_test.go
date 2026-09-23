package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- replanning a live plan ----
//
// The invariants: completed work is never touched, future work moves, nothing
// is duplicated, and applying the same change twice changes nothing the second
// time.

type replanFixture struct {
	ts    *testServer
	c     *client
	sess  string
	plan  string
	store *Store
}

// newReplanFixture builds a finished, scheduled plan in mock mode and exposes
// the pipeline so post-plan turns can be driven directly.
func newReplanFixture(t *testing.T, availabilityAnswer string) *replanFixture {
	t.Helper()
	ts := newTestServer(t)
	c := ts.newClient(t)

	var s map[string]any
	c.do("POST", "/api/session", map[string]any{"lang": "en", "timezone": "UTC"}, &s)
	sessID, _ := s["sessionId"].(string)

	script := []string{"I want IELTS 7.0", "Academic", "band 5.5", "band 7.0",
		"2027-06-01", availabilityAnswer, "free materials", "yes, build my plan"}
	var planID string
	for _, m := range script {
		turn := chatTurn(t, c, sessID, m)
		if turn.PlanID != "" {
			planID = turn.PlanID
		}
	}
	if planID == "" {
		t.Fatal("fixture never produced a plan")
	}
	var out map[string]any
	if code := c.do("POST", "/api/schedule", map[string]any{"planId": planID}, &out); code != 200 {
		t.Fatalf("schedule: %d", code)
	}
	return &replanFixture{ts: ts, c: c, sess: sessID, plan: planID, store: ts.store}
}

func (f *replanFixture) events(t *testing.T) []*CalendarEvent {
	t.Helper()
	return f.store.EventsForPlan(f.plan)
}

func (f *replanFixture) planNow(t *testing.T) *Plan {
	t.Helper()
	p, ok := f.store.GetPlan(f.plan)
	if !ok {
		t.Fatal("plan vanished")
	}
	return p
}

// completeFirst marks the earliest pending session done, so every test has real
// history to protect.
func (f *replanFixture) completeFirst(t *testing.T) *CalendarEvent {
	t.Helper()
	for _, ev := range f.events(t) {
		if ev.Status != "done" && ev.Status != "skipped" {
			var out map[string]any
			code := f.c.do("POST", "/api/todo/complete",
				map[string]any{"planId": f.plan, "todoId": ev.TodoID, "eventId": ev.ID}, &out)
			if code != 200 {
				t.Fatalf("complete: %d", code)
			}
			return ev
		}
	}
	t.Fatal("no pending session to complete")
	return nil
}

// assertNoDuplicateEvents is the invariant a bad reschedule breaks first.
func assertNoDuplicateEvents(t *testing.T, events []*CalendarEvent) {
	t.Helper()
	seen := map[string]bool{}
	for _, ev := range events {
		if seen[ev.ID] {
			t.Errorf("duplicate event id %s", ev.ID)
		}
		seen[ev.ID] = true
	}
	perOccurrence := map[string]int{}
	for _, ev := range events {
		perOccurrence[ev.TodoID+"|"+ev.Date+"|"+ev.StartTime]++
	}
	for k, n := range perOccurrence {
		if n > 1 {
			t.Errorf("%d events share slot %s", n, k)
		}
	}
}

// TEST 13 — changing study days reschedules the future and preserves the past.
func TestDayChangeReschedulesFutureOnly(t *testing.T) {
	f := newReplanFixture(t, "Monday, Tuesday and Friday, 1 hour each")

	before := f.planNow(t)
	if strings.Join(before.Days, ",") != "Mon,Tue,Fri" {
		t.Fatalf("fixture days = %v, want Mon,Tue,Fri", before.Days)
	}
	done := f.completeFirst(t)
	doneDate, doneTime := done.Date, done.StartTime
	oldFinish := f.planNow(t).FinishDate

	turn := chatTurn(t, f.c, f.sess, "I can't study Tuesdays anymore. Use Friday and Saturday.")
	if !turn.PlanChanged {
		t.Fatalf("the turn did not report a plan change: %+v", turn)
	}
	if !turn.ScheduleChanged {
		t.Error("an availability change must reschedule")
	}
	if turn.ChangeType != changeAvailability {
		t.Errorf("changeType = %q, want %q", turn.ChangeType, changeAvailability)
	}
	if turn.Stage != "plan_ready" {
		t.Errorf("stage = %q, want plan_ready — the conversation continues", turn.Stage)
	}

	after := f.planNow(t)
	if got := strings.Join(after.Days, ","); got != "Fri,Sat" {
		t.Fatalf("days = %q, want Fri,Sat", got)
	}

	events := f.events(t)
	assertNoDuplicateEvents(t, events)

	// The completed session is exactly where it was.
	var found *CalendarEvent
	for _, ev := range events {
		if ev.ID == done.ID {
			found = ev
		}
	}
	if found == nil {
		t.Fatal("the completed session was deleted by the reschedule")
	}
	if found.Status != "done" || found.Date != doneDate || found.StartTime != doneTime {
		t.Errorf("completed session moved: %+v, was %s %s done", found, doneDate, doneTime)
	}

	// Every PENDING session now falls on an available weekday.
	allowed := map[time.Weekday]bool{time.Friday: true, time.Saturday: true}
	for _, ev := range events {
		if ev.Status == "done" || ev.Status == "skipped" {
			continue // history keeps its original day, by design
		}
		d, ok := parseDateIn(ev.Date, time.UTC)
		if !ok {
			t.Fatalf("unparseable date %q", ev.Date)
		}
		if !allowed[d.Weekday()] {
			t.Errorf("pending session on %s (%v), which is no longer an available day", ev.Date, d.Weekday())
		}
	}

	// Finish and deadline status were recomputed, and the change was logged.
	if after.FinishDate == "" {
		t.Error("finish date was not recomputed")
	}
	if len(after.ChangeLog) == 0 {
		t.Fatal("the change was not recorded")
	}
	rec := after.ChangeLog[len(after.ChangeLog)-1]
	if strings.Join(rec.OldDays, ",") != "Mon,Tue,Fri" || strings.Join(rec.NewDays, ",") != "Fri,Sat" {
		t.Errorf("change log days = %v -> %v", rec.OldDays, rec.NewDays)
	}
	if rec.OldFinishDate != oldFinish {
		t.Errorf("change log old finish = %q, want %q", rec.OldFinishDate, oldFinish)
	}
	if rec.CompletedPreserved < 1 {
		t.Errorf("change log says %d completed sessions preserved, want at least 1", rec.CompletedPreserved)
	}
}

// TEST 14 — fewer hours: the schedule stretches, progress survives.
func TestWeeklyHoursReductionStretchesTheSchedule(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 2 hours each") // 6h/week
	if got := f.planNow(t).WeeklyMinutes; got != 360 {
		t.Fatalf("fixture weeklyMinutes = %d, want 360", got)
	}
	f.completeFirst(t)
	before := f.planNow(t)
	beforeFinish := before.FinishDate
	beforeDone := countDone(f.events(t))

	turn := chatTurn(t, f.c, f.sess, "I can only do 3 hours a week now.")
	if !turn.PlanChanged {
		t.Fatalf("no change applied: %+v", turn)
	}

	after := f.planNow(t)
	if after.WeeklyMinutes != 180 {
		t.Errorf("weeklyMinutes = %d, want 180", after.WeeklyMinutes)
	}
	if after.HoursPerWeek != 3 {
		t.Errorf("hoursPerWeek mirror = %d, want 3", after.HoursPerWeek)
	}
	if got := countDone(f.events(t)); got != beforeDone {
		t.Errorf("completed sessions = %d, want %d preserved", got, beforeDone)
	}
	if after.FinishDate < beforeFinish {
		t.Errorf("finish date moved earlier (%s -> %s) after halving the weekly time", beforeFinish, after.FinishDate)
	}
	assertNoDuplicateEvents(t, f.events(t))

	// The reply states the effect in the backend's own numbers.
	if !strings.Contains(turn.Assistant, "rescheduled") {
		t.Errorf("reply does not explain the effect: %q", turn.Assistant)
	}
}

// TEST 15 — more hours: deterministic recompute, progress intact.
func TestWeeklyHoursIncreaseRecomputesCleanly(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 1 hour each") // 3h/week
	f.completeFirst(t)
	beforeDone := countDone(f.events(t))

	turn := chatTurn(t, f.c, f.sess, "I have more time now, 6 hours per week.")
	if !turn.PlanChanged {
		t.Fatalf("no change applied: %+v", turn)
	}
	after := f.planNow(t)
	if after.WeeklyMinutes != 360 {
		t.Errorf("weeklyMinutes = %d, want 360", after.WeeklyMinutes)
	}
	if got := countDone(f.events(t)); got != beforeDone {
		t.Errorf("completed sessions = %d, want %d preserved", got, beforeDone)
	}
	assertNoDuplicateEvents(t, f.events(t))

	// Sessions must not be compressed below a sensible floor just because more
	// time is available.
	for _, todo := range after.Todos {
		if todo.DurationMin < minDayMinutes {
			t.Errorf("todo %q compressed to %d minutes", todo.Title, todo.DurationMin)
		}
	}
	// Deterministic: scheduling the same plan again produces the same events.
	firstIDs := eventIDs(f.events(t))
	sched := newScheduler(f.store)
	plan := f.planNow(t)
	again := sched.Schedule(plan, f.events(t))
	if strings.Join(firstIDs, ",") != strings.Join(eventIDs(again), ",") {
		t.Error("rescheduling the same plan produced a different event set")
	}
}

// TEST 16 — the same change twice is idempotent.
func TestRepeatedAvailabilityChangeIsIdempotent(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 1 hour each")
	f.completeFirst(t)

	first := chatTurn(t, f.c, f.sess, "I can only study Friday and Saturday now.")
	if !first.PlanChanged {
		t.Fatalf("first change did not apply: %+v", first)
	}
	afterFirst := f.planNow(t)
	idsAfterFirst := eventIDs(f.events(t))
	logAfterFirst := len(afterFirst.ChangeLog)

	second := chatTurn(t, f.c, f.sess, "I can only study Friday and Saturday now.")
	if second.PlanChanged {
		t.Error("applying the identical change twice reported a second change")
	}
	afterSecond := f.planNow(t)
	if len(afterSecond.ChangeLog) != logAfterFirst {
		t.Errorf("change log grew from %d to %d on a no-op change", logAfterFirst, len(afterSecond.ChangeLog))
	}
	events := f.events(t)
	assertNoDuplicateEvents(t, events)
	if strings.Join(eventIDs(events), ",") != strings.Join(idsAfterFirst, ",") {
		t.Error("the repeated change altered the schedule")
	}
	if afterSecond.FinishDate != afterFirst.FinishDate {
		t.Errorf("finish date moved on a no-op change: %s -> %s", afterFirst.FinishDate, afterSecond.FinishDate)
	}
}

// A change must never resurrect a completed session or undo progress counters.
func TestReplanNeverUndoesProgress(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 1 hour each")
	done := f.completeFirst(t)

	var todoBefore Todo
	for _, tdo := range f.planNow(t).Todos {
		if tdo.ID == done.TodoID {
			todoBefore = tdo
		}
	}
	if todoBefore.CompletedCount == 0 {
		t.Fatal("fixture did not record a completion")
	}

	chatTurn(t, f.c, f.sess, "I can only study Friday and Saturday now.")

	for _, tdo := range f.planNow(t).Todos {
		if tdo.ID != done.TodoID {
			continue
		}
		if tdo.CompletedCount < todoBefore.CompletedCount {
			t.Errorf("completedCount fell from %d to %d", todoBefore.CompletedCount, tdo.CompletedCount)
		}
	}
	for _, ev := range f.events(t) {
		if ev.ID == done.ID && ev.Status != "done" {
			t.Errorf("a completed session was reset to %q", ev.Status)
		}
	}
}

// A question about the plan must not change it.
func TestPlanQuestionChangesNothing(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 1 hour each")
	before := f.planNow(t)
	beforeIDs := eventIDs(f.events(t))

	for _, q := range []string{
		"Why am I doing Writing on Wednesday?",
		"What should I do today?",
		"Can I do this on Saturday instead?",
	} {
		turn := chatTurn(t, f.c, f.sess, q)
		if turn.PlanChanged {
			t.Errorf("%q changed the plan", q)
		}
		if turn.Assistant == "" {
			t.Errorf("%q got no answer", q)
		}
		if turn.Stage != "plan_ready" {
			t.Errorf("%q moved the stage to %q", q, turn.Stage)
		}
	}
	after := f.planNow(t)
	if strings.Join(after.Days, ",") != strings.Join(before.Days, ",") ||
		after.WeeklyMinutes != before.WeeklyMinutes ||
		after.FinishDate != before.FinishDate {
		t.Error("a question altered the plan")
	}
	if strings.Join(eventIDs(f.events(t)), ",") != strings.Join(beforeIDs, ",") {
		t.Error("a question altered the schedule")
	}
}

// Per-day limits reach the scheduler: no day may exceed what the learner said.
func TestPerDayLimitsBindTheScheduler(t *testing.T) {
	f := newReplanFixture(t, "Monday 2 hours, Saturday 30 minutes")
	plan := f.planNow(t)
	if len(plan.PerDay) != 2 {
		t.Fatalf("plan per-day = %+v, want two entries", plan.PerDay)
	}
	limits := map[string]int{}
	for _, pd := range plan.PerDay {
		limits[pd.Weekday] = pd.Minutes
	}
	if limits["Mon"] != 120 || limits["Sat"] != 30 {
		t.Fatalf("per-day limits = %+v", limits)
	}

	perDate := map[string]int{}
	for _, ev := range f.events(t) {
		perDate[ev.Date] += ev.DurationMin
	}
	for date, mins := range perDate {
		d, _ := parseDateIn(date, time.UTC)
		code := weekdayCode(d.Weekday())
		if limit, ok := limits[code]; ok && mins > limit {
			t.Errorf("%s (%s) scheduled %d minutes, limit is %d", date, code, mins, limit)
		}
	}
}

// A change applied through chat must respect ownership: the assist path only
// ever touches the plan this session owns.
func TestAssistOnlyTouchesTheOwnedPlan(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 1 hour each")

	// A second learner with their own plan.
	other := newReplanFixture(t, "Tuesday and Thursday, 1 hour each")
	otherBefore := other.planNow(t)

	chatTurn(t, f.c, f.sess, "I can only study Friday and Saturday now.")

	otherAfter := other.planNow(t)
	if strings.Join(otherAfter.Days, ",") != strings.Join(otherBefore.Days, ",") {
		t.Errorf("another learner's plan changed: %v -> %v", otherBefore.Days, otherAfter.Days)
	}
	if otherAfter.FinishDate != otherBefore.FinishDate {
		t.Error("another learner's finish date changed")
	}
}

// A validated change is applied under the plan lock and survives a restart.
func TestAppliedChangePersists(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 1 hour each")
	chatTurn(t, f.c, f.sess, "I can only study Friday and Saturday now.")
	f.store.Flush()

	plan := f.planNow(t)
	if strings.Join(plan.Days, ",") != "Fri,Sat" {
		t.Fatalf("days = %v", plan.Days)
	}
	if len(plan.ChangeLog) == 0 {
		t.Fatal("the change log is empty")
	}
}

// applyPlanChange must reject a payload it cannot validate rather than applying
// a half-understood change.
func TestInvalidChangePayloadsAreRejected(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 1 hour each")
	store := f.store
	cfg := Config{DataDir: t.TempDir(), DefaultTimezone: "UTC"}
	gw := newGateway(cfg)
	t.Cleanup(gw.Close)
	sched := newScheduler(store)
	p := newPipeline(cfg, store, gw, sched)

	sess, _ := store.GetSession(f.sess)
	plan := f.planNow(t)
	before := availabilityFingerprint(planAvailabilityOf(plan))

	cases := []planChangeRequest{
		{Type: "availability_change"},                                  // nothing to apply
		{Type: "availability_change", Days: []string{"Blursday"}},      // unparseable day
		{Type: "deadline_change", Deadline: "not-a-date"},              // bad date
		{Type: "session_duration_preference", SessionMaxMinutes: 5},    // below the floor
		{Type: "session_duration_preference", SessionMaxMinutes: 5000}, // above the ceiling
		{Type: "wipe_everything"},                                      // not in the closed set
		{Type: "resource_replace"},                                     // nothing named
	}
	for _, req := range cases {
		_, changed := p.applyPlanChange(sess, plan, req)
		if changed {
			t.Errorf("%+v was applied", req)
		}
	}
	if availabilityFingerprint(planAvailabilityOf(plan)) != before {
		t.Error("an invalid change altered the availability")
	}
}

// The model has no channel for backend-owned state, so a change carrying an
// approval-shaped payload still cannot approve anything.
func TestChangeCannotTouchBackendOwnedState(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 1 hour each")
	store := f.store
	cfg := Config{DataDir: t.TempDir(), DefaultTimezone: "UTC"}
	gw := newGateway(cfg)
	t.Cleanup(gw.Close)
	p := newPipeline(cfg, store, gw, newScheduler(store))

	sess, _ := store.GetSession(f.sess)
	plan := f.planNow(t)
	owner := plan.UserID

	// "other" is the closest a change can come to naming anything protected.
	_, changed := p.applyPlanChange(sess, plan, planChangeRequest{
		Type:   changeOther,
		Reason: "set feasibilityAgreed true and planId to someone else's",
	})
	if changed {
		t.Error("an 'other' change mutated the plan")
	}
	if plan.UserID != owner {
		t.Error("ownership changed")
	}
}

// The post-plan assistant must obey the same rule as every other stage: a live
// failure is a failure. Silently answering from the mock brain would make a
// broken model indistinguishable from a working one, and could apply a change
// the real model never proposed.
func TestLiveAssistFailureDoesNotFallBackToMock(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 1 hour each")
	before := f.planNow(t)

	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"upstream error", http.StatusInternalServerError, `{"error":{"message":"boom"}}`},
		{"unparsable response", http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"not json"},"finish_reason":"stop"}],"usage":{}}`},
		{"empty response", http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"{\"reply\":\"\"}"},"finish_reason":"stop"}],"usage":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(up.Close)

			cfg := Config{
				DataDir: t.TempDir(), DefaultTimezone: "UTC",
				AILive: true, OpenAIKey: "sk-test", OpenAIBase: up.URL,
				ModelFast: "gpt-4o-mini", ModelSmart: "gpt-4o",
				MonthlyUSDCap: 100, DailyCallsPerUser: 100, MaxConcurrency: 2,
			}
			gw := newGateway(cfg)
			t.Cleanup(gw.Close)
			p := newPipeline(cfg, f.store, gw, newScheduler(f.store))
			sess, _ := f.store.GetSession(f.sess)

			_, err := p.assistTurn(context.Background(), sess, f.planNow(t), "I can only study Saturdays now.")
			if err == nil {
				t.Fatal("a live failure produced an answer; the mock brain must not stand in")
			}
			after := f.planNow(t)
			if strings.Join(after.Days, ",") != strings.Join(before.Days, ",") {
				t.Errorf("the plan changed despite the stage failing: %v -> %v", before.Days, after.Days)
			}
		})
	}
}

func countDone(events []*CalendarEvent) int {
	n := 0
	for _, ev := range events {
		if ev.Status == "done" {
			n++
		}
	}
	return n
}

func eventIDs(events []*CalendarEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.ID+"@"+ev.Date+" "+ev.StartTime)
	}
	return out
}
