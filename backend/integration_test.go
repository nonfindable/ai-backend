package main

import (
	"io"
	"strings"
	"testing"
)

// P6: the whole live-frontend path, walked once, in mock AI mode so it depends
// on nothing external. The scheduler stays deterministic throughout.

func TestFullUserJourney(t *testing.T) {
	ts := newTestServer(t)
	c := &client{t: t, base: ts.URL}

	// 1. create session
	var sess map[string]any
	if code := c.do("POST", "/api/session", map[string]any{"lang": "en", "timezone": "Asia/Tashkent"}, &sess); code != 200 {
		t.Fatalf("create session: %d", code)
	}
	c.token, _ = sess["token"].(string)
	c.user, _ = sess["userId"].(string)
	sessionID, _ := sess["sessionId"].(string)
	if c.token == "" || c.user == "" || sessionID == "" {
		t.Fatalf("session response incomplete: %+v", sess)
	}
	if tz, _ := sess["timezone"].(string); tz != "Asia/Tashkent" {
		t.Errorf("session timezone = %q, want Asia/Tashkent", tz)
	}

	// 2/3. start chat and complete the intake
	var planID string
	var lastStage string
	// The last message is the go-ahead: nothing is built until the user
	// approves the recap at the confirm_plan stage.
	script := []string{"I want IELTS 7.0", "Academic", "band 5.5", "band 7.0",
		"2026-12-01", "10 hours a week on Mon Wed Fri", "free materials",
		"yes, build my plan"}
	for _, msg := range script {
		turn := chatTurn(t, c, sessionID, msg)
		lastStage = turn.Stage
		if turn.PlanID != "" {
			planID = turn.PlanID
		}
		if turn.Stage == "intake" && turn.Progress == nil {
			t.Error("intake turn without a progress block")
		}
	}

	// 4. receive plan ID
	if planID == "" {
		t.Fatalf("intake never produced a plan (last stage %q)", lastStage)
	}
	if lastStage != "plan_ready" {
		t.Errorf("final stage = %q, want plan_ready", lastStage)
	}

	// 5. fetch plan
	var plan Plan
	if code := c.do("GET", "/api/plan/"+planID, nil, &plan); code != 200 {
		t.Fatalf("fetch plan: %d", code)
	}
	if plan.Timezone != "Asia/Tashkent" {
		t.Errorf("plan timezone = %q", plan.Timezone)
	}
	if len(plan.Todos) == 0 {
		t.Fatal("plan has no todos")
	}
	if plan.Deadline != "2026-12-01" {
		t.Errorf("plan deadline = %q, want the one given at intake", plan.Deadline)
	}

	// 6. generate schedule
	var schedOut map[string]any
	if code := c.do("POST", "/api/schedule", map[string]any{"planId": planID}, &schedOut); code != 200 {
		t.Fatalf("schedule: %d", code)
	}
	scheduled := int(schedOut["count"].(float64))
	if scheduled == 0 {
		t.Fatal("scheduling produced no sessions")
	}

	// 7. confirm the schedule
	var confirmOut map[string]any
	if code := c.do("POST", "/api/schedule/confirm", map[string]any{"planId": planID}, &confirmOut); code != 200 {
		t.Fatalf("confirm: %d", code)
	}
	if int(confirmOut["confirmed"].(float64)) == 0 {
		t.Error("confirm promoted nothing")
	}

	// 8. fetch calendar
	var events []CalendarEvent
	if code := c.do("GET", "/api/calendar?planId="+planID, nil, &events); code != 200 {
		t.Fatalf("calendar: %d", code)
	}
	if len(events) != scheduled {
		t.Fatalf("calendar has %d events, schedule reported %d", len(events), scheduled)
	}
	for _, e := range events {
		if e.Status != "scheduled" {
			t.Errorf("event %s is %q after confirm, want scheduled", e.ID, e.Status)
		}
	}

	// 9. complete ONE session of a recurring todo
	var target CalendarEvent
	counts := map[string]int{}
	for _, e := range events {
		counts[e.TodoID]++
	}
	for _, e := range events {
		if counts[e.TodoID] >= 2 {
			target = e
			break
		}
	}
	if target.ID == "" {
		t.Fatal("no recurring todo to test partial completion")
	}
	seriesLen := counts[target.TodoID]

	var done map[string]any
	if code := c.do("POST", "/api/todo/complete", map[string]any{
		"planId": planID, "todoId": target.TodoID, "eventId": target.ID,
	}, &done); code != 200 {
		t.Fatalf("complete: %d", code)
	}

	// 10. verify ONLY that session is completed and the series survives
	if got := int(done["completedCount"].(float64)); got != 1 {
		t.Errorf("completedCount = %d, want 1", got)
	}
	if status, _ := done["status"].(string); status != "in_progress" {
		t.Errorf("todo status = %q, want in_progress — one session must not finish the series", status)
	}
	if got := int(done["remaining"].(float64)); got != seriesLen-1 {
		t.Errorf("remaining = %d, want %d", got, seriesLen-1)
	}

	c.do("GET", "/api/calendar?planId="+planID, nil, &events)
	doneCount := 0
	for _, e := range events {
		if e.TodoID != target.TodoID {
			continue
		}
		if e.Status == "done" {
			doneCount++
			if e.ID != target.ID {
				t.Errorf("the wrong session was completed: %s", e.ID)
			}
		}
	}
	if doneCount != 1 {
		t.Errorf("%d sessions of the series are done, want exactly 1", doneCount)
	}

	// 11. roll a missed session forward
	before := len(events)
	beforeIDs := map[string]bool{}
	for _, e := range events {
		beforeIDs[e.ID] = true
	}
	var rollOut map[string]any
	if code := c.do("POST", "/api/rollover", map[string]any{"asOf": "2027-01-01"}, &rollOut); code != 200 {
		t.Fatalf("rollover: %d", code)
	}
	results, _ := rollOut["results"].([]any)
	if len(results) == 0 {
		t.Error("rollover reported no work despite every session being in the past")
	}

	// 12. verify no duplicates and no invented events
	var after []CalendarEvent
	c.do("GET", "/api/calendar?planId="+planID, nil, &after)
	if len(after) != before {
		t.Errorf("rollover changed the event count: %d -> %d", before, len(after))
	}
	seen := map[string]bool{}
	for _, e := range after {
		if seen[e.ID] {
			t.Errorf("duplicate event %s after rollover", e.ID)
		}
		seen[e.ID] = true
		if !beforeIDs[e.ID] {
			t.Errorf("rollover invented event %s", e.ID)
		}
	}
	// The completed session must not have been dragged forward.
	for _, e := range after {
		if e.ID == target.ID {
			if e.Status != "done" {
				t.Errorf("completed session lost its status in rollover: %q", e.Status)
			}
			if e.Date != target.Date {
				t.Errorf("completed session was moved: %s -> %s", target.Date, e.Date)
			}
		}
	}

	// 13. export ICS
	resp := c.request("GET", "/api/plan/"+planID+"/ics", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("ics export: %d", resp.StatusCode)
	}
	ics := string(body)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/calendar") {
		t.Errorf("ics content-type = %q", ct)
	}

	// 14. verify the ICS contains the expected events
	if !strings.HasPrefix(ics, "BEGIN:VCALENDAR") || !strings.Contains(ics, "END:VCALENDAR") {
		t.Fatal("ics is not a well-formed calendar")
	}
	if n := strings.Count(ics, "BEGIN:VEVENT"); n != len(after) {
		t.Errorf("ics has %d VEVENTs, calendar has %d events", n, len(after))
	}
	if strings.Count(ics, "BEGIN:VEVENT") != strings.Count(ics, "END:VEVENT") {
		t.Error("unbalanced VEVENT blocks")
	}
	for _, e := range after {
		if !strings.Contains(ics, "UID:"+e.ID+"@start.ai") {
			t.Errorf("ics is missing event %s", e.ID)
		}
	}
	if !strings.Contains(ics, "DTSTAMP:") {
		t.Error("ics has no DTSTAMP; strict parsers reject it")
	}
	// The completed session must be exported as COMPLETED.
	if !strings.Contains(ics, "STATUS:COMPLETED") {
		t.Error("the completed session was not exported as COMPLETED")
	}
	for _, line := range strings.Split(ics, "\r\n") {
		if len(line) > 75 {
			t.Errorf("unfolded line exceeds the RFC 5545 limit (%d octets): %q", len(line), line)
		}
	}
}

// The same journey must be reproducible: identical inputs, identical schedule.
func TestJourneyIsDeterministic(t *testing.T) {
	run := func() []string {
		ts := newTestServer(t)
		c := ts.newClient(t)
		_, planID := c.buildPlan()
		c.do("POST", "/api/schedule", map[string]any{"planId": planID}, nil)
		var evs []CalendarEvent
		c.do("GET", "/api/calendar?planId="+planID, nil, &evs)
		out := []string{}
		for _, e := range evs {
			// Plan IDs differ between runs, so compare the schedule shape.
			out = append(out, e.Title+"|"+e.Date+"|"+e.StartTime+"|"+itoa(e.DurationMin))
		}
		return out
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatalf("two identical journeys produced %d and %d sessions", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("session %d differs between runs:\n  %s\n  %s", i, a[i], b[i])
		}
	}
}
