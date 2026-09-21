package main

import (
	"strings"
	"testing"
	"time"
)

func testPlan(t *testing.T) *Plan {
	t.Helper()
	loc := loadLocation("UTC")
	start := todayIn(loc).AddDate(0, 0, 1)
	p := &Plan{
		ID: "plan_test", UserID: "user_test", Skill: "IELTS", Lang: "en",
		Timezone: "UTC", HoursPerWeek: 10, WeeksTotal: 12,
		Days:      []string{"Mon", "Tue", "Wed", "Thu", "Fri"},
		StartDate: dateStr(start),
		Phases: []Phase{
			{Key: "fundamentals", WeekStart: 1, WeekEnd: 2},
			{Key: "drills", WeekStart: 3, WeekEnd: 10},
			{Key: "rehearsal", WeekStart: 11, WeekEnd: 12},
		},
		Todos: []Todo{
			{ID: "t_diag", Title: "Diagnostic", DurationMin: 120, Frequency: "once", Priority: "high", Phase: "fundamentals", Status: "pending"},
			{ID: "t_read", Title: "Reading", DurationMin: 60, Frequency: "twice_weekly", Priority: "high", Phase: "drills", Status: "pending"},
			{ID: "t_write", Title: "Writing", DurationMin: 60, Frequency: "twice_weekly", Priority: "high", Phase: "drills", Status: "pending"},
			{ID: "t_mock", Title: "Mock exam", DurationMin: 165, Frequency: "weekly", Priority: "high", Phase: "rehearsal", Status: "pending"},
		},
	}
	return p
}

func eventsByTodo(evs []*CalendarEvent, todoID string) []*CalendarEvent {
	var out []*CalendarEvent
	for _, e := range evs {
		if e.TodoID == todoID {
			out = append(out, e)
		}
	}
	return out
}

func weekOf(t *testing.T, plan *Plan, date string) int {
	t.Helper()
	loc := loadLocation(plan.Timezone)
	anchor, _ := parseDateIn(plan.StartDate, loc)
	d, ok := parseDateIn(date, loc)
	if !ok {
		t.Fatalf("bad date %q", date)
	}
	return daysBetween(anchor, d) / 7
}

// The stated cadence must produce that many sessions per week, not a single
// consecutive block of every occurrence.
func TestScheduleHonoursCadence(t *testing.T) {
	plan := testPlan(t)
	events := (&Scheduler{}).Schedule(plan, nil)

	perWeek := map[int]int{}
	for _, e := range eventsByTodo(events, "t_read") {
		perWeek[weekOf(t, plan, e.Date)]++
	}
	if len(perWeek) < 6 {
		t.Errorf("twice_weekly todo spans only %d weeks; expected it spread across its 8-week phase", len(perWeek))
	}
	for w, n := range perWeek {
		if n > 2 {
			t.Errorf("week %d has %d sessions of a twice_weekly todo, want at most 2", w, n)
		}
	}
}

// The user's weekly hour budget is a constraint, not decoration.
func TestScheduleRespectsWeeklyHourBudget(t *testing.T) {
	plan := testPlan(t)
	events := (&Scheduler{}).Schedule(plan, nil)

	perWeek := map[int]int{}
	for _, e := range events {
		perWeek[weekOf(t, plan, e.Date)] += e.DurationMin
	}
	budget := plan.HoursPerWeek * 60
	for w, mins := range perWeek {
		if mins > budget {
			t.Errorf("week %d schedules %d min, over the %d min budget", w, mins, budget)
		}
	}
}

// Rehearsal work belongs in the rehearsal weeks.
func TestScheduleRespectsPhaseWindows(t *testing.T) {
	plan := testPlan(t)
	events := (&Scheduler{}).Schedule(plan, nil)

	for _, e := range eventsByTodo(events, "t_mock") {
		if w := weekOf(t, plan, e.Date); w < 10 {
			t.Errorf("rehearsal session landed in week %d, before its phase window (weeks 11-12 => index 10-11)", w)
		}
	}
	for _, e := range eventsByTodo(events, "t_diag") {
		if w := weekOf(t, plan, e.Date); w > 1 {
			t.Errorf("fundamentals session landed in week %d, after its phase window", w)
		}
	}
}

func TestScheduleGivesEveryTodoSessions(t *testing.T) {
	plan := testPlan(t)
	events := (&Scheduler{}).Schedule(plan, nil)
	for _, todo := range plan.Todos {
		if len(eventsByTodo(events, todo.ID)) == 0 {
			t.Errorf("todo %q got no sessions at all", todo.Title)
		}
	}
	if plan.DroppedSessions > 0 {
		t.Logf("note: %d sessions did not fit the weekly budget (reported, not silently dropped)", plan.DroppedSessions)
	}
}

func TestScheduleNeverExceedsPerDayLimits(t *testing.T) {
	plan := testPlan(t)
	events := (&Scheduler{}).Schedule(plan, nil)
	perDayCount := map[string]int{}
	perDayMin := map[string]int{}
	for _, e := range events {
		perDayCount[e.Date]++
		perDayMin[e.Date] += e.DurationMin
	}
	// 10 h/week over 5 days, so no day should hold an unreasonable block.
	budget := plan.HoursPerWeek * 60
	limit := dayMinuteCap(budget, len(plan.Days), 0, dayStartFor(budget, len(plan.Days)))
	for d, n := range perDayCount {
		if n > maxSessionsPerDay {
			t.Errorf("%s has %d sessions, max is %d", d, n, maxSessionsPerDay)
		}
		if perDayMin[d] > limit {
			t.Errorf("%s schedules %d min, over the %d min per-day cap", d, perDayMin[d], limit)
		}
	}
}

func TestScheduleOnlyUsesAvailableDays(t *testing.T) {
	plan := testPlan(t)
	plan.Days = []string{"Sat", "Sun"}
	events := (&Scheduler{}).Schedule(plan, nil)
	loc := loadLocation(plan.Timezone)
	for _, e := range events {
		d, _ := parseDateIn(e.Date, loc)
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			t.Errorf("session on %s (%v), but only weekends are available", e.Date, wd)
		}
	}
}

// Completing one session of a recurring todo must not cancel the remainder.
func TestCompletingOneSessionKeepsTheSeries(t *testing.T) {
	plan := testPlan(t)
	events := (&Scheduler{}).Schedule(plan, nil)

	before := len(eventsByTodo(events, "t_read"))
	if before < 3 {
		t.Fatalf("expected several Reading sessions, got %d", before)
	}

	// Mark the first Reading session done, as the complete endpoint does.
	first := eventsByTodo(events, "t_read")[0]
	first.Status = "done"
	for i := range plan.Todos {
		if plan.Todos[i].ID == "t_read" {
			plan.Todos[i].CompletedCount = 1
			syncTodoStatus(&plan.Todos[i])
		}
	}

	after := (&Scheduler{}).Schedule(plan, events)
	remaining := 0
	done := 0
	for _, e := range eventsByTodo(after, "t_read") {
		if e.Status == "done" {
			done++
		} else {
			remaining++
		}
	}
	if done != 1 {
		t.Errorf("completed session count = %d, want 1", done)
	}
	if remaining < before-2 {
		t.Errorf("after completing 1 of %d sessions only %d remain; the series was cancelled", before, remaining)
	}
}

func TestScheduleChecksTheDeadline(t *testing.T) {
	plan := testPlan(t)
	loc := loadLocation(plan.Timezone)
	// A deadline two weeks out cannot hold a 12-week plan.
	plan.Deadline = dateStr(todayIn(loc).AddDate(0, 0, 14))
	(&Scheduler{}).Schedule(plan, nil)
	if !plan.MissesDeadline {
		t.Error("a 12-week plan against a 2-week deadline should be flagged")
	}
	if plan.DeadlineSlipDays <= 0 {
		t.Errorf("DeadlineSlipDays = %d, want a positive slip", plan.DeadlineSlipDays)
	}
	if plan.DeadlineNote == "" {
		t.Error("a missed deadline should carry an explanatory note")
	}

	plan2 := testPlan(t)
	plan2.Deadline = dateStr(todayIn(loc).AddDate(0, 0, 400))
	(&Scheduler{}).Schedule(plan2, nil)
	if plan2.MissesDeadline {
		t.Error("a comfortable deadline should not be flagged")
	}
}

func TestScheduleIsDeterministicForTheSameInput(t *testing.T) {
	a := testPlan(t)
	b := testPlan(t)
	ea := (&Scheduler{}).Schedule(a, nil)
	eb := (&Scheduler{}).Schedule(b, nil)
	if len(ea) != len(eb) {
		t.Fatalf("event counts differ: %d vs %d", len(ea), len(eb))
	}
	for i := range ea {
		if ea[i].Date != eb[i].Date || ea[i].TodoID != eb[i].TodoID || ea[i].StartTime != eb[i].StartTime {
			t.Fatalf("run %d differs: %s/%s/%s vs %s/%s/%s", i,
				ea[i].Date, ea[i].TodoID, ea[i].StartTime, eb[i].Date, eb[i].TodoID, eb[i].StartTime)
		}
	}
}

// ---- rollover ----

func rolloverFixture(t *testing.T) (*Scheduler, *Store, *Plan, string) {
	t.Helper()
	dir := t.TempDir()
	store := newStore(dir)
	t.Cleanup(store.Close)

	plan := testPlan(t)
	sched := newScheduler(store)
	events := sched.Schedule(plan, nil)
	store.SavePlan(plan)
	store.ReplaceEventsForPlan(plan.ID, events)
	return sched, store, plan, plan.UserID
}

func TestRolloverPreservesUnconfirmedStatus(t *testing.T) {
	sched, store, plan, userID := rolloverFixture(t)
	loc := loadLocation(plan.Timezone)

	// Nothing done; pretend five weeks have passed.
	ref := todayIn(loc).AddDate(0, 0, 35)
	if got := sched.Rollover(userID, ref); len(got) == 0 {
		t.Fatal("expected a rollover summary")
	}
	for _, ev := range store.EventsForPlan(plan.ID) {
		if ev.Status == "scheduled" {
			t.Error("a proposed session was silently promoted to scheduled by rollover")
		}
	}
}

func TestRolloverShiftsMilestonesWithTheFinishDate(t *testing.T) {
	dir := t.TempDir()
	store := newStore(dir)
	defer store.Close()

	// Only one usable day a week, so eight weeks of missed sessions cannot be
	// absorbed inside the original span and the finish date must move.
	loc := loadLocation("UTC")
	start := todayIn(loc).AddDate(0, 0, 1)
	plan := &Plan{
		ID: "plan_slip", UserID: "user_slip", Skill: "Chess", Lang: "en",
		Timezone: "UTC", HoursPerWeek: 10, WeeksTotal: 12,
		Days: []string{"Mon"}, StartDate: dateStr(start),
		Phases: []Phase{{Key: "drills", WeekStart: 1, WeekEnd: 12}},
		Todos: []Todo{
			{ID: "t_drill", Title: "Drill", DurationMin: 60, Frequency: "twice_weekly", Priority: "high", Phase: "drills", Status: "pending"},
		},
		Milestones: []Milestone{
			{ID: "ms1", Title: "midpoint", TargetDate: dateStr(start.AddDate(0, 0, 42))},
			{ID: "ms2", Title: "final", TargetDate: dateStr(start.AddDate(0, 0, 84))},
		},
	}
	sched := newScheduler(store)
	events := sched.Schedule(plan, nil)
	store.SavePlan(plan)
	store.ReplaceEventsForPlan(plan.ID, events)

	beforeMid := plan.Milestones[0].TargetDate
	beforeFinal := plan.Milestones[1].TargetDate

	summaries := sched.Rollover(plan.UserID, todayIn(loc).AddDate(0, 0, 56))
	if len(summaries) == 0 {
		t.Fatal("expected a rollover summary")
	}
	shift := summaries[0].FinishShifts
	if shift <= 0 {
		t.Fatalf("expected the finish date to slip, got shift=%d", shift)
	}

	updated, _ := store.GetPlan(plan.ID)
	if updated.Milestones[1].TargetDate == beforeFinal {
		t.Errorf("finish moved +%d days but the final milestone stayed at %s", shift, beforeFinal)
	}
	// A milestone already in the past is history and must not be rewritten.
	if updated.Milestones[0].TargetDate != beforeMid {
		t.Errorf("a milestone that already passed was moved from %s to %s", beforeMid, updated.Milestones[0].TargetDate)
	}
	if updated.OriginalFinishDate == updated.FinishDate {
		t.Error("OriginalFinishDate should still record the pre-slip commitment")
	}
}

func TestRolloverLeavesCompletedSessionsAlone(t *testing.T) {
	sched, store, plan, userID := rolloverFixture(t)
	loc := loadLocation(plan.Timezone)

	events := store.EventsForPlan(plan.ID)
	events[0].Status = "done"
	doneDate := events[0].Date
	store.SaveEvents(events)

	sched.Rollover(userID, todayIn(loc).AddDate(0, 0, 35))
	for _, ev := range store.EventsForPlan(plan.ID) {
		if ev.Status == "done" && ev.Date != doneDate {
			t.Errorf("a completed session was moved from %s to %s", doneDate, ev.Date)
		}
	}
}

// ---- ICS ----

func icsFor(t *testing.T, lang string) (string, *Plan) {
	t.Helper()
	plan := testPlan(t)
	plan.Lang = lang
	if lang == "ru" {
		plan.Skill = "Гитара"
		for i := range plan.Todos {
			plan.Todos[i].Title = "Диагностический пробный тест и подробный разбор всех ошибок"
		}
	}
	events := (&Scheduler{}).Schedule(plan, nil)
	return (&Scheduler{}).ICS(plan, events), plan
}

func TestICSHasMandatoryFields(t *testing.T) {
	out, _ := icsFor(t, "en")
	for _, want := range []string{"BEGIN:VCALENDAR", "VERSION:2.0", "BEGIN:VEVENT", "UID:", "DTSTAMP:", "DTSTART:", "DTEND:", "SUMMARY:", "END:VCALENDAR"} {
		if !strings.Contains(out, want) {
			t.Errorf("ICS output is missing %q", want)
		}
	}
}

func TestICSUsesUnambiguousUTCTimes(t *testing.T) {
	out, _ := icsFor(t, "en")
	for _, line := range strings.Split(out, "\r\n") {
		if strings.HasPrefix(line, "DTSTART:") || strings.HasPrefix(line, "DTEND:") {
			if !strings.HasSuffix(line, "Z") {
				t.Errorf("%q is a floating time; it must be UTC or carry a TZID", line)
			}
		}
	}
}

func TestICSFoldsLongLinesIncludingCyrillic(t *testing.T) {
	for _, lang := range []string{"en", "ru"} {
		out, _ := icsFor(t, lang)
		for _, line := range strings.Split(out, "\r\n") {
			if len(line) > 75 {
				t.Errorf("lang=%s: line of %d octets exceeds the RFC 5545 fold limit: %.40q", lang, len(line), line)
			}
		}
		// Unfolding must restore the original text.
		unfolded := strings.ReplaceAll(out, "\r\n ", "")
		if !strings.Contains(unfolded, "SUMMARY:") {
			t.Errorf("lang=%s: folding corrupted the output", lang)
		}
	}
}

func TestICSEscapesSpecialCharacters(t *testing.T) {
	plan := testPlan(t)
	plan.Todos[0].Title = "Review; compare, contrast\nand \\ escape"
	events := (&Scheduler{}).Schedule(plan, nil)
	out := (&Scheduler{}).ICS(plan, events)
	unfolded := strings.ReplaceAll(out, "\r\n ", "")
	if !strings.Contains(unfolded, `Review\; compare\, contrast\nand \\ escape`) {
		t.Errorf("special characters were not escaped:\n%s", unfolded)
	}
	// A raw newline inside a value would break the line structure.
	for _, line := range strings.Split(out, "\r\n") {
		if strings.Contains(line, "\n") {
			t.Error("a raw newline survived into a content line")
		}
	}
}

func TestICSSkipsUnparseableDates(t *testing.T) {
	plan := testPlan(t)
	out := (&Scheduler{}).ICS(plan, []*CalendarEvent{{ID: "x", Date: "not-a-date", Title: "x"}})
	if strings.Contains(out, "BEGIN:VEVENT") {
		t.Error("an event with an invalid date should be skipped, not emitted")
	}
}

// A realistic IELTS week (8 sessions across 3 available days) must still fit
// inside its phase window. A fixed 2-sessions-per-day cap used to make this
// impossible, silently pushing drills and rehearsal past the end of the plan.
func TestDensePlanStillFitsItsPhaseWindows(t *testing.T) {
	loc := loadLocation("UTC")
	start := todayIn(loc).AddDate(0, 0, 1)
	plan := &Plan{
		ID: "plan_dense", UserID: "u", Skill: "IELTS", Lang: "en", Timezone: "UTC",
		HoursPerWeek: 10, WeeksTotal: 12, Days: []string{"Mon", "Wed", "Fri"},
		StartDate: dateStr(start),
		Phases: []Phase{
			{Key: "fundamentals", WeekStart: 1, WeekEnd: 2},
			{Key: "drills", WeekStart: 3, WeekEnd: 10},
			{Key: "rehearsal", WeekStart: 11, WeekEnd: 12},
		},
		Todos: []Todo{
			{ID: "d", Title: "Diagnostic", DurationMin: 120, Frequency: "once", Priority: "high", Phase: "fundamentals"},
			{ID: "r", Title: "Reading", DurationMin: 60, Frequency: "twice_weekly", Priority: "high", Phase: "drills"},
			{ID: "w", Title: "Writing", DurationMin: 60, Frequency: "twice_weekly", Priority: "high", Phase: "drills"},
			{ID: "s", Title: "Speaking", DurationMin: 30, Frequency: "thrice_weekly", Priority: "medium", Phase: "drills"},
			{ID: "l", Title: "Listening", DurationMin: 45, Frequency: "weekly", Priority: "medium", Phase: "drills"},
			{ID: "m", Title: "Mock", DurationMin: 165, Frequency: "weekly", Priority: "high", Phase: "rehearsal"},
		},
	}
	events := (&Scheduler{}).Schedule(plan, nil)

	windows := map[string][2]int{}
	for _, ph := range plan.Phases {
		windows[ph.Key] = [2]int{ph.WeekStart, ph.WeekEnd}
	}
	byTodo := map[string]*Todo{}
	for i := range plan.Todos {
		byTodo[plan.Todos[i].ID] = &plan.Todos[i]
	}
	for _, e := range events {
		todo := byTodo[e.TodoID]
		win := windows[todo.Phase]
		if w := weekOf(t, plan, e.Date) + 1; w < win[0] || w > win[1] {
			t.Errorf("%s (%s) scheduled in week %d, outside its window %d-%d",
				todo.Title, todo.Phase, w, win[0], win[1])
		}
	}
	if plan.DroppedSessions > 0 {
		t.Errorf("a plan that fits the hour budget dropped %d sessions", plan.DroppedSessions)
	}
	// The schedule must not run past the plan's own length.
	last := weekOf(t, plan, events[len(events)-1].Date) + 1
	if last > plan.WeeksTotal {
		t.Errorf("schedule ends in week %d of a %d-week plan", last, plan.WeeksTotal)
	}
}

// A heavy week must not run into the night. 14 hours across three days is
// nearly five hours a day, which does not fit the default 18:00 evening slot —
// anchoring to it regardless pushed the last session past midnight.
func TestScheduleKeepsSessionsInsideTheDayWindow(t *testing.T) {
	plan := testPlan(t)
	plan.HoursPerWeek = 14
	plan.Days = []string{"Mon", "Tue", "Fri"}

	events := (&Scheduler{}).Schedule(plan, nil)
	if len(events) == 0 {
		t.Fatal("nothing was scheduled")
	}
	for _, e := range events {
		start := startMinuteOf(e.StartTime)
		if start < dayFloorMinute {
			t.Errorf("%s %s starts before the %s floor", e.Date, e.StartTime, formatMinute(dayFloorMinute))
		}
		if end := start + e.DurationMin; end > dayEndMinute {
			t.Errorf("%s %s runs to %s, past the %s cutoff",
				e.Date, e.StartTime, formatMinute(end), formatMinute(dayEndMinute))
		}
	}
}

// Every session must land on a day the user actually chose, and the week must
// spread over those days rather than packing the first one to its cap.
func TestScheduleSpreadsOverTheChosenDays(t *testing.T) {
	plan := testPlan(t)
	plan.HoursPerWeek = 14
	plan.Days = []string{"Mon", "Tue", "Fri"}
	want := parseWeekdaySet(plan.Days)

	events := (&Scheduler{}).Schedule(plan, nil)
	loc := loadLocation(plan.Timezone)
	used := map[time.Weekday]int{}
	for _, e := range events {
		d, ok := parseDateIn(e.Date, loc)
		if !ok {
			t.Fatalf("event %s has an unparsable date %q", e.ID, e.Date)
		}
		if !want[d.Weekday()] {
			t.Errorf("%s falls on %s, which the user did not choose", e.Date, d.Weekday())
		}
		used[d.Weekday()]++
	}
	for wd := range want {
		if used[wd] == 0 {
			t.Errorf("%s was chosen but never used; the week is not spreading", wd)
		}
	}
}
