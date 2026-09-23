package main

import (
	"strings"
	"testing"
)

// ---- resource memory and substitution ----
//
// The learner does not always use what was recommended. What matters is that
// the plan remembers what they ARE using, that an equivalent swap costs nothing
// but a relabel, and that only a genuinely different workload touches the
// learning content.

func planWithResources(t *testing.T) *Plan {
	t.Helper()
	plan := &Plan{
		ID: "plan_1", UserID: "u1", Skill: "IELTS", Timezone: "UTC", Lang: "en",
		StartDate: "2026-01-05", WeeksTotal: 8, HoursPerWeek: 6, WeeklyMinutes: 360,
		Days: []string{"Mon", "Wed", "Fri"},
		Phases: []Phase{
			{Key: "drills", Title: "Drills", WeekStart: 1, WeekEnd: 8},
		},
		Todos: []Todo{
			{ID: "t1", PlanID: "plan_1", Title: "Reading practice", DurationMin: 60,
				Frequency: "twice_weekly", Priority: "high", Phase: "drills",
				ResourceRef: "Cambridge IELTS 19", Status: "pending"},
			{ID: "t2", PlanID: "plan_1", Title: "Speaking drills", DurationMin: 30,
				Frequency: "weekly", Priority: "medium", Phase: "drills",
				ResourceRef: "", Status: "pending"},
		},
		SetupItems: []SetupItem{
			{ID: "i1", PlanID: "plan_1", Name: "Cambridge IELTS 19", Category: "materials",
				Priority: "high", PriceRange: "$0-25", Rationale: "full-length practice tests"},
		},
	}
	seedResources(plan)
	return plan
}

// Seeding records what was recommended, so there is something to replace.
func TestSeedResourcesRecordsRecommendations(t *testing.T) {
	plan := planWithResources(t)
	if len(plan.Resources) == 0 {
		t.Fatal("no resources were recorded")
	}
	found := false
	for _, r := range plan.Resources {
		if r.Recommended == "Cambridge IELTS 19" {
			found = true
			if r.InUse() != "Cambridge IELTS 19" {
				t.Errorf("inUse = %q, want the recommendation until the learner says otherwise", r.InUse())
			}
			if r.Source != "recommended" {
				t.Errorf("source = %q, want recommended", r.Source)
			}
		}
	}
	if !found {
		t.Errorf("the recommended book was not recorded: %+v", plan.Resources)
	}
	// A name appearing in both a setup item and a todo is recorded once.
	seen := map[string]int{}
	for _, r := range plan.Resources {
		seen[normalizeResourceKey(r.Recommended)]++
	}
	if seen[normalizeResourceKey("Cambridge IELTS 19")] != 1 {
		t.Errorf("the same resource was recorded %d times", seen[normalizeResourceKey("Cambridge IELTS 19")])
	}
}

// TEST 17 — an equivalent swap relabels and nothing else.
func TestEquivalentResourceSwapDoesNotReplan(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 1 hour each")
	plan := f.planNow(t)
	if len(plan.Resources) == 0 {
		t.Skip("the mock plan recorded no resources to swap")
	}
	original := plan.Resources[0].InUse()
	beforeIDs := eventIDs(f.events(t))
	beforeFinish := plan.FinishDate

	// Drive the change directly so the substitution is unambiguous.
	cfg := Config{DataDir: t.TempDir(), DefaultTimezone: "UTC"}
	gw := newGateway(cfg)
	t.Cleanup(gw.Close)
	p := newPipeline(cfg, f.store, gw, newScheduler(f.store))
	sess, _ := f.store.GetSession(f.sess)

	rec, changed := p.applyPlanChange(sess, plan, planChangeRequest{
		Type:            changeResourceReplace,
		ResourceFrom:    original,
		ResourceTo:      "Cambridge IELTS 18",
		Equivalent:      true,
		WorkloadChanged: false,
	})
	if !changed {
		t.Fatal("the swap was not applied")
	}
	if rec.Rescheduled {
		t.Error("an equivalent swap must not reschedule anything")
	}

	after := f.planNow(t)
	if after.FinishDate != beforeFinish {
		t.Errorf("finish date moved on an equivalent swap: %s -> %s", beforeFinish, after.FinishDate)
	}
	if strings.Join(eventIDs(f.events(t)), ",") != strings.Join(beforeIDs, ",") {
		t.Error("an equivalent swap changed the schedule")
	}

	// The selection moved, the recommendation is remembered, the old one is
	// recorded as rejected.
	var sel *ResourceSelection
	for i := range after.Resources {
		if after.Resources[i].InUse() == "Cambridge IELTS 18" {
			sel = &after.Resources[i]
		}
	}
	if sel == nil {
		t.Fatalf("the selection did not move: %+v", after.Resources)
	}
	if sel.Recommended != original {
		t.Errorf("recommended = %q, want the original %q remembered", sel.Recommended, original)
	}
	if !containsStr(sel.Rejected, original) {
		t.Errorf("rejected = %v, want it to contain %q", sel.Rejected, original)
	}
	if sel.Source != "user" {
		t.Errorf("source = %q, want user", sel.Source)
	}
}

// Todo references follow the substitution.
func TestResourceSwapRepointsTodos(t *testing.T) {
	plan := planWithResources(t)
	sel := findResource(plan, "Cambridge IELTS 19")
	if sel == nil {
		t.Fatal("could not find the recommended resource")
	}
	touched := replaceResource(plan, sel, "Cambridge IELTS 18", true, "same series, previous edition")
	if touched == 0 {
		t.Error("nothing was repointed")
	}
	for _, todo := range plan.Todos {
		if todo.ResourceRef == "Cambridge IELTS 19" {
			t.Errorf("todo %q still points at the old resource", todo.Title)
		}
	}
	if plan.Todos[0].ResourceRef != "Cambridge IELTS 18" {
		t.Errorf("todo resourceRef = %q, want the new book", plan.Todos[0].ResourceRef)
	}
	if plan.SetupItems[0].Name != "Cambridge IELTS 18" {
		t.Errorf("setup item = %q, want the new book", plan.SetupItems[0].Name)
	}
}

// TEST 18 — a materially different workload updates only the affected work.
func TestWorkloadChangingSwapUpdatesAffectedWorkOnly(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 2 hours each")
	plan := f.planNow(t)
	if len(plan.Resources) == 0 {
		t.Skip("the mock plan recorded no resources to swap")
	}
	// Pick a resource that at least one todo actually references.
	var target string
	for _, r := range plan.Resources {
		for _, todo := range plan.Todos {
			if todo.ResourceRef != "" && resourceOverlap(todo.ResourceRef, r.InUse()) >= 0.8 {
				target = r.InUse()
			}
		}
	}
	if target == "" {
		t.Skip("no todo references a recorded resource in this mock plan")
	}

	// Remember every todo that does NOT use the resource.
	untouched := map[string]int{}
	for _, todo := range plan.Todos {
		if resourceOverlap(todo.ResourceRef, target) < 0.8 {
			untouched[todo.ID] = todo.DurationMin
		}
	}

	cfg := Config{DataDir: t.TempDir(), DefaultTimezone: "UTC"}
	gw := newGateway(cfg)
	t.Cleanup(gw.Close)
	p := newPipeline(cfg, f.store, gw, newScheduler(f.store))
	sess, _ := f.store.GetSession(f.sess)

	rec, changed := p.applyPlanChange(sess, plan, planChangeRequest{
		Type:            changeResourceReplace,
		ResourceFrom:    target,
		ResourceTo:      "Complete IELTS Bands 6.5-7.5 (4-book bundle)",
		Equivalent:      false,
		WorkloadChanged: true,
		SessionMinutes:  90,
	})
	if !changed {
		t.Fatal("the swap was not applied")
	}
	if !rec.Rescheduled {
		t.Error("a workload change must recompute the schedule")
	}

	after := f.planNow(t)
	for _, todo := range after.Todos {
		if want, ok := untouched[todo.ID]; ok && todo.DurationMin != want {
			t.Errorf("unrelated todo %q changed duration %d -> %d", todo.Title, want, todo.DurationMin)
		}
		if strings.EqualFold(todo.ResourceRef, "Complete IELTS Bands 6.5-7.5 (4-book bundle)") && todo.DurationMin != 90 {
			t.Errorf("affected todo %q duration = %d, want 90", todo.Title, todo.DurationMin)
		}
	}
	assertNoDuplicateEvents(t, f.events(t))
	if rec.CompletedPreserved != countDone(f.events(t)) {
		t.Errorf("change log preserved count %d disagrees with the calendar", rec.CompletedPreserved)
	}
}

// TEST 19 — later questions are answered against the SELECTED resource.
func TestPlanContextNamesTheSelectedResource(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 1 hour each")
	plan := f.planNow(t)
	if len(plan.Resources) == 0 {
		t.Skip("the mock plan recorded no resources")
	}
	original := plan.Resources[0].InUse()

	cfg := Config{DataDir: t.TempDir(), DefaultTimezone: "UTC"}
	gw := newGateway(cfg)
	t.Cleanup(gw.Close)
	p := newPipeline(cfg, f.store, gw, newScheduler(f.store))
	sess, _ := f.store.GetSession(f.sess)

	if _, changed := p.applyPlanChange(sess, plan, planChangeRequest{
		Type: changeResourceReplace, ResourceFrom: original,
		ResourceTo: "Cambridge IELTS 18", Equivalent: true,
	}); !changed {
		t.Fatal("swap not applied")
	}

	ctxPayload := p.buildAssistContext(sess, f.planNow(t))
	if !strings.Contains(ctxPayload, "Cambridge IELTS 18") {
		t.Errorf("the assistant context does not name the selected resource:\n%s", ctxPayload)
	}
	if !strings.Contains(ctxPayload, `"inUse":"Cambridge IELTS 18"`) {
		t.Error("the context must mark which resource is actually in use")
	}
	// The mock answer must use it too.
	answer := mockPlanAnswer(sess, f.planNow(t), "which book am i using?")
	if !strings.Contains(answer, "Cambridge IELTS 18") {
		t.Errorf("answer = %q, want it to name the selected book", answer)
	}
	if strings.Contains(answer, original) && original != "Cambridge IELTS 18" {
		t.Errorf("answer still names the superseded recommendation: %q", answer)
	}
}

// Dropping a resource clears the references that pointed at it.
func TestRemovingAResourceClearsItsReferences(t *testing.T) {
	plan := planWithResources(t)
	sel := findResource(plan, "Cambridge IELTS 19")
	if sel == nil {
		t.Fatal("resource not found")
	}
	removeResource(plan, sel)
	for _, todo := range plan.Todos {
		if todo.ResourceRef == "Cambridge IELTS 19" {
			t.Errorf("todo %q still references the removed resource", todo.Title)
		}
	}
	if findResource(plan, "Cambridge IELTS 19") != nil {
		t.Error("the resource is still recorded")
	}
}

// A name that matches nothing must not silently rewrite the wrong resource.
func TestFindResourceRefusesAWeakMatch(t *testing.T) {
	plan := planWithResources(t)
	if got := findResource(plan, "a bicycle pump"); got != nil {
		t.Errorf("matched %q to an unrelated request", got.Recommended)
	}
	if got := findResource(plan, ""); got != nil {
		t.Error("an empty name matched something")
	}
	if got := findResource(plan, "cambridge ielts 19"); got == nil {
		t.Error("a case-different exact name did not match")
	}
}

// ---- plan questions ----

// TEST 28 — a question about a scheduled session gets the context to answer it.
func TestAssistContextCarriesTheScheduleForWhyQuestions(t *testing.T) {
	f := newReplanFixture(t, "Monday, Wednesday and Friday, 1 hour each")
	cfg := Config{DataDir: t.TempDir(), DefaultTimezone: "UTC"}
	gw := newGateway(cfg)
	t.Cleanup(gw.Close)
	p := newPipeline(cfg, f.store, gw, newScheduler(f.store))
	sess, _ := f.store.GetSession(f.sess)
	plan := f.planNow(t)

	payload := p.buildAssistContext(sess, plan)

	for _, want := range []string{
		`"upcomingSessions"`, `"phases"`, `"activePhase"`, `"todos"`,
		`"resources"`, `"availability"`, `"finishDate"`, `"todaySessions"`,
		`"recentChanges"`, `"deadline"`,
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("assist context is missing %s", want)
		}
	}
	// A session must carry its weekday and its phase, which is what "why am I
	// doing this on Wednesday?" is actually asking about.
	if !strings.Contains(payload, `"weekday"`) {
		t.Error("sessions carry no weekday, so a day-of-week question is unanswerable")
	}
	if !strings.Contains(payload, `"phase"`) {
		t.Error("sessions carry no phase, so the reason for the work is unavailable")
	}
	// And it must stay compact: the whole plan is not pasted in.
	if len(payload) > 60_000 {
		t.Errorf("assist context is %d bytes; it is meant to be compact", len(payload))
	}
}

// TEST 29 — "what should I do today?" receives today's sessions.
func TestAssistContextCarriesTodaysSessions(t *testing.T) {
	f := newReplanFixture(t, "Monday, Tuesday, Wednesday, Thursday, Friday, Saturday and Sunday, 1 hour each")
	cfg := Config{DataDir: t.TempDir(), DefaultTimezone: "UTC"}
	gw := newGateway(cfg)
	t.Cleanup(gw.Close)
	p := newPipeline(cfg, f.store, gw, newScheduler(f.store))
	sess, _ := f.store.GetSession(f.sess)
	plan := f.planNow(t)

	// The plan starts tomorrow, so shift one event onto today to prove the
	// field is populated from the calendar rather than always empty.
	_, todayStr := planToday(plan)
	events := f.store.EventsForPlan(plan.ID)
	if len(events) == 0 {
		t.Fatal("no events scheduled")
	}
	events[0].Date = todayStr
	f.store.SaveEvents(events)

	payload := p.buildAssistContext(sess, plan)
	if strings.Contains(payload, `"todaySessions":[]`) || strings.Contains(payload, `"todaySessions":null`) {
		t.Errorf("today's sessions were not carried:\n%s", payload)
	}
	if !strings.Contains(payload, todayStr) {
		t.Errorf("today's date %s does not appear in the context", todayStr)
	}
	if got := p.sessionsOn(plan, todayStr); len(got) == 0 {
		t.Error("sessionsOn found nothing for today")
	}
}
