package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---- life after plan_ready ----
//
// A finished plan is the beginning of the relationship, not the end of it. The
// learner comes back to ask why something is in the plan, what to do today,
// whether a different book will do — and to tell start.ai that Tuesdays are
// gone now. All of that arrives on the same POST /api/chat.
//
// The division of labour is the same as everywhere else:
//
//	the model   reads the message, resolves references against the transcript,
//	            and proposes ONE structured change from a closed set
//	the backend validates that change and applies it
//	the scheduler picks the actual dates
//
// The model never sees a plan object it could mutate. It sees a compact,
// read-only summary and answers with a planChangeRequest.

// assistResult is the assist stage's output shape.
type assistResult struct {
	// Reply is the answer to show the user. The backend appends its own factual
	// line about anything it actually changed, because the numbers in that
	// sentence have to be the numbers that were applied.
	Reply  string            `json:"reply"`
	Change planChangeRequest `json:"change"`
}

// assistContextEvents bounds how much calendar goes into the prompt.
const (
	assistContextDays   = 14
	assistContextEvents = 25
	assistContextTodos  = 20
)

// buildAssistContext assembles the compact authoritative plan state.
//
// Compact on purpose: a full plan with a year of events would crowd out the
// conversation and cost a fortune on every "what's today?". This carries the
// fields post-plan questions actually need, and the calendar only for the
// fortnight around now.
func (p *Pipeline) buildAssistContext(sess *IntakeSession, plan *Plan) string {
	loc := loadLocation(plan.Timezone)
	today := todayIn(loc)
	todayStr := dateStr(today)
	horizonStr := dateStr(today.AddDate(0, 0, assistContextDays))

	av := planAvailabilityOf(plan)

	// The phase that is running now, by week offset from the start date.
	activePhase := ""
	if start, ok := parseDateIn(plan.StartDate, loc); ok {
		week := daysBetween(start, today)/7 + 1
		for _, ph := range plan.Phases {
			if week >= ph.WeekStart && week <= ph.WeekEnd {
				activePhase = ph.Key
				break
			}
		}
	}

	type eventView struct {
		ID       string `json:"id"`
		Date     string `json:"date"`
		Weekday  string `json:"weekday"`
		Start    string `json:"startTime"`
		Minutes  int    `json:"durationMin"`
		Title    string `json:"title"`
		TodoID   string `json:"todoId"`
		Phase    string `json:"phase"`
		Status   string `json:"status"`
		Resource string `json:"resourceRef,omitempty"`
	}
	todoByID := map[string]*Todo{}
	for i := range plan.Todos {
		todoByID[plan.Todos[i].ID] = &plan.Todos[i]
	}

	var todayEvents, upcoming []eventView
	for _, ev := range p.store.EventsForPlan(plan.ID) {
		if ev.Date < todayStr || ev.Date > horizonStr {
			continue
		}
		v := eventView{
			ID: ev.ID, Date: ev.Date, Start: ev.StartTime, Minutes: ev.DurationMin,
			Title: ev.Title, TodoID: ev.TodoID, Status: ev.Status,
		}
		if d, ok := parseDateIn(ev.Date, loc); ok {
			v.Weekday = d.Weekday().String()
		}
		if t := todoByID[ev.TodoID]; t != nil {
			v.Phase = t.Phase
			v.Resource = t.ResourceRef
		}
		if ev.Date == todayStr {
			todayEvents = append(todayEvents, v)
		}
		if len(upcoming) < assistContextEvents {
			upcoming = append(upcoming, v)
		}
	}

	type todoView struct {
		Title     string `json:"title"`
		Minutes   int    `json:"durationMin"`
		Frequency string `json:"frequency"`
		Phase     string `json:"phase"`
		Status    string `json:"status"`
		Resource  string `json:"resourceRef,omitempty"`
		Done      int    `json:"completed"`
		Planned   int    `json:"planned"`
	}
	todos := make([]todoView, 0, minInt(len(plan.Todos), assistContextTodos))
	for i, t := range plan.Todos {
		if i >= assistContextTodos {
			break
		}
		todos = append(todos, todoView{
			Title: t.Title, Minutes: t.DurationMin, Frequency: t.Frequency,
			Phase: t.Phase, Status: t.Status, Resource: t.ResourceRef,
			Done: t.CompletedCount, Planned: t.PlannedCount,
		})
	}

	type resourceView struct {
		Need        string `json:"need"`
		Recommended string `json:"recommended,omitempty"`
		InUse       string `json:"inUse"`
		Kind        string `json:"kind,omitempty"`
	}
	resources := make([]resourceView, 0, len(plan.Resources))
	for _, r := range plan.Resources {
		resources = append(resources, resourceView{
			Need: r.Need, Recommended: r.Recommended, InUse: r.InUse(), Kind: r.Kind,
		})
	}

	// The last few changes, so "why did my finish date move?" is answerable
	// from fact rather than reconstructed from the schedule.
	recent := plan.ChangeLog
	if len(recent) > 5 {
		recent = recent[len(recent)-5:]
	}

	b, _ := json.Marshal(map[string]any{
		"today":        todayStr,
		"timezone":     plan.Timezone,
		"skill":        plan.Skill,
		"path":         plan.Path,
		"currentLevel": sess.Answers.CurrentLevel,
		"target":       sess.Answers.Target,
		"deadline":     plan.Deadline,
		"availability": map[string]any{
			"days":          av.Days,
			"weeklyMinutes": av.WeeklyMinutes,
			"hoursPerWeek":  av.HoursPerWeek(),
			"perDay":        av.PerDay,
		},
		"planId":            plan.ID,
		"startDate":         plan.StartDate,
		"finishDate":        plan.FinishDate,
		"weeksTotal":        plan.WeeksTotal,
		"feasibilityStatus": plan.FeasibilityStatus,
		"missesDeadline":    plan.MissesDeadline,
		"deadlineSlipDays":  plan.DeadlineSlipDays,
		"droppedSessions":   plan.DroppedSessions,
		"phases":            plan.Phases,
		"activePhase":       activePhase,
		"milestones":        plan.Milestones,
		"todos":             todos,
		"resources":         resources,
		"todaySessions":     todayEvents,
		"upcomingSessions":  upcoming,
		"recentChanges":     recent,
		"userRequests":      sess.PlanNotes,
	})
	return string(b)
}

// doAssist handles one post-plan message.
func (p *Pipeline) doAssist(ctx context.Context, sess *IntakeSession, msg string) (Turn, error) {
	plan, ok := p.store.GetPlan(sess.PlanID)
	if !ok || plan.UserID != sess.UserID {
		// The plan this session built is gone, or never belonged to this user.
		// Ownership is checked here, not inferred from the session, and a
		// mismatch falls back to treating the message as a fresh goal rather
		// than handing over someone else's plan.
		sess.Stage = "scope_check"
		sess.PlanID = ""
		return p.doUnderstand(ctx, sess, msg)
	}

	// Everything below mutates the plan and its calendar, so it runs under the
	// plan's mutation lock — the same one the schedule, confirm, complete and
	// rollover paths take. Lock order is user then plan; the caller holds only
	// the session lock, so there is no cycle.
	unlock := p.sched.planLocks.Lock(plan.ID)
	defer unlock()

	// Re-read under the lock: the copy above is stale the moment it is returned.
	plan, ok = p.store.GetPlan(sess.PlanID)
	if !ok || plan.UserID != sess.UserID {
		sess.Stage = "scope_check"
		sess.PlanID = ""
		return p.doUnderstand(ctx, sess, msg)
	}

	r, err := p.assistTurn(ctx, sess, plan, msg)
	if err != nil {
		return Turn{}, err
	}

	changeType := normalizeChangeType(r.Change.Type)

	// A new target is not a tweak: it needs a different plan, and the learner
	// has to agree to it first. Reopening the confirmation gate reuses the one
	// piece of machinery that already does recap, arithmetic and approval,
	// instead of growing a second path that could approve a plan by itself.
	if changeType == changeTarget && strings.TrimSpace(r.Change.Target) != "" {
		sess.Answers.Target = strings.TrimSpace(r.Change.Target)
		sess.FeasibilityAgreed = false
		sess.FeasibilityStatus = ""
		turn, gerr := p.gateBeforePlan(ctx, sess)
		if gerr != nil {
			return Turn{}, gerr
		}
		turn.ChangeType = changeTarget
		if reply := strings.TrimSpace(r.Reply); reply != "" {
			turn.Assistant = reply + "\n\n" + turn.Assistant
		}
		return turn, nil
	}

	// Enrichment, strictly optional. When a marketplace provider is configured
	// we try to attach a VERIFIED listing for the resource the learner named;
	// when none is (the default), or the lookup fails, nothing happens and the
	// change still applies. A price or a link is never invented to fill the gap.
	p.enrichResourceChange(ctx, &r.Change)

	rec, changed := p.applyPlanChange(sess, plan, r.Change)

	reply := strings.TrimSpace(r.Reply)
	if effect := changeEffectLine(sess.Lang, rec); effect != "" {
		reply = strings.TrimSpace(reply + "\n\n" + effect)
	}
	if reply == "" {
		reply = tr(sess.Lang,
			"I've got your plan open — ask me about any part of it, or tell me what changed.",
			"Ваш план у меня перед глазами — спросите о любой его части или скажите, что изменилось.",
			"Rejangiz oldimda — istalgan qismi haqida so'rang yoki nima o'zgarganini ayting.")
	}

	return Turn{
		Stage:           "plan_ready",
		Assistant:       reply,
		PlanID:          plan.ID,
		Done:            true,
		PlanChanged:     changed,
		ScheduleChanged: changed && rec.Rescheduled,
		ChangeType:      changeTypeOrEmpty(changed, rec.Type),
	}, nil
}

// enrichResourceChange attaches provider-verified facts to a resource change,
// when and only when a provider actually supplies them.
//
// This is where the division of labour is enforced in code: the model said WHAT
// the learner wants ("Cambridge IELTS 18"); a provider says whether such a
// thing exists, what it costs and where. If no provider is configured — which
// is the shipped default, because none of the real marketplaces has a
// documented API here — this is a no-op and the plan change proceeds untouched.
func (p *Pipeline) enrichResourceChange(ctx context.Context, change *planChangeRequest) {
	if p.res == nil || !p.res.Enabled() {
		return
	}
	switch normalizeChangeType(change.Type) {
	case changeResourceReplace, changeResourceAdd:
	default:
		return
	}
	query := strings.TrimSpace(change.ResourceTo)
	if query == "" {
		return
	}
	lookupCtx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	choice, err := p.res.FindProduct(lookupCtx, ResourceNeed{
		Kind:     change.ResourceKind,
		Query:    query,
		Purpose:  change.Reason,
		Country:  p.cfg.MarketplaceCountry,
		Currency: p.cfg.MarketplaceCurrency,
	})
	if err != nil || choice == nil {
		// No verified result is a perfectly good outcome. It is recorded as
		// "we could not check", never as a recommendation we made up.
		return
	}
	// The exact title the provider returned is more trustworthy than the one
	// the learner typed, so the selection records that.
	change.ResourceTo = choice.Listing.Title
	change.Reason = strings.TrimSpace(change.Reason + " (" + choice.Claim + ")")
}

func changeTypeOrEmpty(changed bool, t string) string {
	if !changed {
		return ""
	}
	return t
}

// assistTurn runs the assist stage, real or mock. In live mode a failure is
// reported; it never silently becomes the mock assistant.
func (p *Pipeline) assistTurn(ctx context.Context, sess *IntakeSession, plan *Plan, msg string) (assistResult, error) {
	if !p.gw.Enabled() {
		return mockAssist(sess, plan, msg), nil
	}
	payload := p.buildAssistContext(sess, plan)
	history := conversationWindow(sess)
	task := "Answer the current user message about this plan. If it asks for a change, describe it in \"change\" using the closed type list; otherwise set change.type to \"plan_question\". You never pick dates — the scheduler does."
	turns := stageTurns(prompts["assist"].System, sess.Lang, history, payload, msg, task)
	raw, err := p.gw.ChatTurns(ctx, sess.UserID, p.cfg.ModelSmart, turns, true,
		cacheKeyForTurns("assist", sess.Lang, payload+"\x00"+msg, history))
	if err != nil {
		return assistResult{}, err
	}
	var r assistResult
	if e := json.Unmarshal([]byte(extractJSONObject(raw)), &r); e != nil {
		return assistResult{}, fmt.Errorf("%w: assist stage returned unparsable JSON: %v", errAIUnavailable, e)
	}
	if strings.TrimSpace(r.Reply) == "" && normalizeChangeType(r.Change.Type) == changeNone {
		return assistResult{}, fmt.Errorf("%w: assist stage returned nothing to show the user", errAIUnavailable)
	}
	return r, nil
}

// planSummaryLine is a short factual status used by the mock assistant and as a
// fallback when there is nothing else to say.
func planSummaryLine(lang string, plan *Plan) string {
	return tr(lang,
		"Your "+plan.Skill+" plan runs "+plan.StartDate+" to "+plan.FinishDate+", "+
			strings.Join(plan.Days, "/")+", about "+itoa(plan.HoursPerWeek)+"h a week.",
		"Ваш план по «"+plan.Skill+"» идёт с "+plan.StartDate+" по "+plan.FinishDate+", "+
			strings.Join(plan.Days, "/")+", около "+itoa(plan.HoursPerWeek)+" ч в неделю.",
		"«"+plan.Skill+"» rejangiz "+plan.StartDate+" dan "+plan.FinishDate+" gacha, "+
			strings.Join(plan.Days, "/")+", haftasiga taxminan "+itoa(plan.HoursPerWeek)+" soat.")
}

// sessionsOn lists what is scheduled on one date, for "what should I do today?".
func (p *Pipeline) sessionsOn(plan *Plan, date string) []*CalendarEvent {
	var out []*CalendarEvent
	for _, ev := range p.store.EventsForPlan(plan.ID) {
		if ev.Date == date && ev.Status != "skipped" {
			out = append(out, ev)
		}
	}
	return out
}

// todayIn for a plan, as a date string.
func planToday(plan *Plan) (time.Time, string) {
	loc := loadLocation(plan.Timezone)
	t := todayIn(loc)
	return t, dateStr(t)
}
