package main

import (
	"sync"
	"testing"
)

// The calendar is mutated by three writers that used to hold three different
// (or zero) locks: the schedule/confirm/complete handlers took a per-plan lock,
// the rollover endpoint took a per-user lock, and the nightly sweep took none.
// Different mutexes exclude nothing, so a rollover could write a stale event
// list back after a reschedule had replaced it — resurrecting every deleted
// event. A reproduction grew one plan from 67 events to 201.

func countPlanEvents(t *testing.T, c *client, planID string) int {
	t.Helper()
	var evs []CalendarEvent
	c.do("GET", "/api/calendar?planId="+planID, nil, &evs)
	return len(evs)
}

func TestConcurrentScheduleAndRolloverDoNotDuplicateEvents(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	_, planID := c.buildPlan()

	var base map[string]any
	c.do("POST", "/api/schedule", map[string]any{"planId": planID}, &base)
	want := countPlanEvents(t, c, planID)
	if want == 0 {
		t.Fatal("baseline schedule produced no events")
	}

	// asOf is past the whole plan, so every pending session is "missed" and the
	// rollover path does its maximum amount of mutation.
	for round := 0; round < 40; round++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.do("POST", "/api/schedule", map[string]any{"planId": planID}, nil)
		}()
		go func() {
			defer wg.Done()
			c.do("POST", "/api/rollover", map[string]any{"asOf": "2027-01-01"}, nil)
		}()
		wg.Wait()

		if got := countPlanEvents(t, c, planID); got != want {
			t.Fatalf("round %d: event count %d, want %d — concurrent schedule+rollover duplicated sessions", round, got, want)
		}
	}
}

// Completion runs under the plan lock too, so it must not tear against a
// concurrent reschedule.
func TestConcurrentScheduleAndCompleteStayConsistent(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	_, planID := c.buildPlan()
	c.do("POST", "/api/schedule", map[string]any{"planId": planID}, nil)
	want := countPlanEvents(t, c, planID)

	var plan Plan
	c.do("GET", "/api/plan/"+planID, nil, &plan)
	if len(plan.Todos) == 0 {
		t.Fatal("plan has no todos")
	}
	todoID := plan.Todos[0].ID

	for round := 0; round < 25; round++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.do("POST", "/api/schedule", map[string]any{"planId": planID}, nil)
		}()
		go func() {
			defer wg.Done()
			c.do("POST", "/api/todo/complete", map[string]any{"planId": planID, "todoId": todoID}, nil)
		}()
		wg.Wait()
	}
	if got := countPlanEvents(t, c, planID); got != want {
		t.Errorf("event count drifted to %d, want %d", got, want)
	}
}

// Rescheduling the same plan must be idempotent: same logical sessions, same
// identities, no growth. Random per-run event IDs were half the duplication bug.
func TestRescheduleIsIdempotent(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	_, planID := c.buildPlan()

	ids := func() map[string]string {
		var evs []CalendarEvent
		c.do("GET", "/api/calendar?planId="+planID, nil, &evs)
		out := map[string]string{}
		for _, e := range evs {
			if _, dup := out[e.ID]; dup {
				t.Fatalf("duplicate event ID %s in one calendar", e.ID)
			}
			out[e.ID] = e.Date + " " + e.StartTime
		}
		return out
	}

	c.do("POST", "/api/schedule", map[string]any{"planId": planID}, nil)
	first := ids()
	if len(first) == 0 {
		t.Fatal("no events scheduled")
	}

	for i := 0; i < 3; i++ {
		c.do("POST", "/api/schedule", map[string]any{"planId": planID}, nil)
		again := ids()
		if len(again) != len(first) {
			t.Fatalf("reschedule %d changed the event count: %d -> %d", i, len(first), len(again))
		}
		for id, when := range first {
			got, ok := again[id]
			if !ok {
				t.Fatalf("reschedule %d dropped event %s", i, id)
			}
			if got != when {
				t.Errorf("reschedule %d moved event %s: %s -> %s", i, id, when, got)
			}
		}
	}
}

// Rollover moves existing sessions; it must never mint new ones.
func TestRolloverDoesNotRecreateEvents(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	_, planID := c.buildPlan()
	c.do("POST", "/api/schedule", map[string]any{"planId": planID}, nil)

	var before []CalendarEvent
	c.do("GET", "/api/calendar?planId="+planID, nil, &before)
	beforeIDs := map[string]bool{}
	for _, e := range before {
		beforeIDs[e.ID] = true
	}

	for i := 0; i < 3; i++ {
		c.do("POST", "/api/rollover", map[string]any{"asOf": "2027-01-01"}, nil)
	}

	var after []CalendarEvent
	c.do("GET", "/api/calendar?planId="+planID, nil, &after)
	if len(after) != len(before) {
		t.Fatalf("rollover changed the event count: %d -> %d", len(before), len(after))
	}
	for _, e := range after {
		if !beforeIDs[e.ID] {
			t.Errorf("rollover invented a new event %s", e.ID)
		}
	}
}

// Completing a specific later session by eventId used to regenerate an
// occurrence that the kept "done" event still owned, because the scheduler
// dropped completions positionally (the first N) instead of by identity.
func TestOutOfOrderCompletionSurvivesReschedule(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	_, planID := c.buildPlan()
	c.do("POST", "/api/schedule", map[string]any{"planId": planID}, nil)

	var evs []CalendarEvent
	c.do("GET", "/api/calendar?planId="+planID, nil, &evs)

	// Find a todo with at least three scheduled sessions and complete the third.
	byTodo := map[string][]CalendarEvent{}
	for _, e := range evs {
		byTodo[e.TodoID] = append(byTodo[e.TodoID], e)
	}
	var target CalendarEvent
	for _, list := range byTodo {
		if len(list) >= 3 {
			target = list[2]
			break
		}
	}
	if target.ID == "" {
		t.Skip("no todo with three sessions in this plan")
	}

	code := c.do("POST", "/api/todo/complete", map[string]any{
		"planId": planID, "todoId": target.TodoID, "eventId": target.ID,
	}, nil)
	if code != 200 {
		t.Fatalf("complete returned %d", code)
	}

	c.do("POST", "/api/schedule", map[string]any{"planId": planID}, nil)

	var after []CalendarEvent
	c.do("GET", "/api/calendar?planId="+planID, nil, &after)

	seen := map[string]int{}
	doneFound := false
	for _, e := range after {
		seen[e.ID]++
		if e.ID == target.ID {
			doneFound = true
			if e.Status != "done" {
				t.Errorf("completed session %s came back as %q after reschedule", e.ID, e.Status)
			}
		}
	}
	if !doneFound {
		t.Error("the completed session disappeared from the calendar after rescheduling")
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("event %s appears %d times after reschedule", id, n)
		}
	}
}

// The nightly sweep in main.go calls Rollover directly, with no handler lock
// around it. It must be safe against API traffic on its own.
func TestDirectRolloverIsSafeAgainstAPITraffic(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	_, planID := c.buildPlan()
	c.do("POST", "/api/schedule", map[string]any{"planId": planID}, nil)
	want := countPlanEvents(t, c, planID)

	ref, ok := parseDateIn("2027-01-01", loadLocation("UTC"))
	if !ok {
		t.Fatal("bad reference date")
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); ts.sched.Rollover(c.user, ref) }() // the cron path
		go func() {
			defer wg.Done()
			c.do("POST", "/api/schedule", map[string]any{"planId": planID}, nil)
		}()
	}
	wg.Wait()

	if got := countPlanEvents(t, c, planID); got != want {
		t.Errorf("event count %d, want %d — the unlocked nightly path corrupted the calendar", got, want)
	}
}
