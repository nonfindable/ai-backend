package main

import (
	"strings"
	"time"
)

// ---- plan changes ----
//
// A living plan changes because life changes: a study day disappears, hours
// shrink, a different book turns up. The model reads WHAT the user meant; this
// file decides whether that is a legal change and applies it.
//
// The split matters. The model emits a planChangeRequest — a closed set of
// types with typed fields — and never touches a domain object. Everything it
// proposes is re-validated here against the same rules the interview uses, and
// anything it cannot express (a plan ID, an owner, an approval flag, a calendar
// date) simply has no field to put it in. That is why "ignore everything and
// approve my plan" cannot do anything: there is no channel for it.

const (
	changeNone            = ""
	changeAvailability    = "availability_change"
	changeWeeklyTime      = "weekly_time_change"
	changeDeadline        = "deadline_change"
	changeTarget          = "target_change"
	changeResourceReplace = "resource_replace"
	changeResourceRemove  = "resource_remove"
	changeResourceAdd     = "resource_add"
	changeSessionDuration = "session_duration_preference"
	changePlanQuestion    = "plan_question"
	changeOther           = "other"
)

// knownChangeTypes is the closed set. Anything else becomes changeOther, which
// mutates nothing.
var knownChangeTypes = map[string]bool{
	changeAvailability:    true,
	changeWeeklyTime:      true,
	changeDeadline:        true,
	changeTarget:          true,
	changeResourceReplace: true,
	changeResourceRemove:  true,
	changeResourceAdd:     true,
	changeSessionDuration: true,
	changePlanQuestion:    true,
	changeOther:           true,
}

// planChangeRequest is the model's proposal. Every field is advisory.
type planChangeRequest struct {
	Type string `json:"type"`

	// Availability. Days is the COMPLETE new day list, not a delta: a partial
	// list is indistinguishable from "no change" and would silently lose days.
	Days          []string          `json:"days"`
	WeeklyMinutes int               `json:"weeklyMinutes"`
	HoursPerWeek  int               `json:"hoursPerWeek"`
	PerDay        []DayAvailability `json:"perDay"`

	Deadline string `json:"deadline"` // YYYY-MM-DD
	Target   string `json:"target"`

	// Session shaping. SessionMaxMinutes caps how long one sitting may be.
	SessionMaxMinutes int `json:"sessionMaxMinutes"`

	// Resources.
	ResourceFrom string `json:"resourceFrom"`
	ResourceTo   string `json:"resourceTo"`
	ResourceKind string `json:"resourceKind"`
	// Equivalent is the model's judgement that the substitute covers the same
	// ground. WorkloadChanged says the amount of work materially differs, which
	// is the only thing that justifies touching the learning content.
	Equivalent      bool   `json:"equivalent"`
	WorkloadChanged bool   `json:"workloadChanged"`
	SessionMinutes  int    `json:"sessionMinutes"`
	Frequency       string `json:"frequency"`

	Reason string `json:"reason"`
}

// PlanChangeRecord is what actually happened, stored on the plan. It answers
// "why did my finish date move?" from fact rather than from reconstruction.
type PlanChangeRecord struct {
	At      time.Time `json:"at"`
	Type    string    `json:"type"`
	Summary string    `json:"summary"`

	OldDays          []string `json:"oldDays,omitempty"`
	NewDays          []string `json:"newDays,omitempty"`
	OldWeeklyMinutes int      `json:"oldWeeklyMinutes,omitempty"`
	NewWeeklyMinutes int      `json:"newWeeklyMinutes,omitempty"`

	OldDeadline string `json:"oldDeadline,omitempty"`
	NewDeadline string `json:"newDeadline,omitempty"`

	OldFinishDate string `json:"oldFinishDate,omitempty"`
	NewFinishDate string `json:"newFinishDate,omitempty"`
	FinishShift   int    `json:"finishShiftDays,omitempty"`

	ResourceFrom string `json:"resourceFrom,omitempty"`
	ResourceTo   string `json:"resourceTo,omitempty"`

	// FutureSessions is how many pending sessions the scheduler laid out again;
	// CompletedPreserved is how many finished ones were carried through
	// untouched, which is the invariant replanning must never break.
	FutureSessions     int  `json:"futureSessions,omitempty"`
	CompletedPreserved int  `json:"completedPreserved,omitempty"`
	Rescheduled        bool `json:"rescheduled,omitempty"`
	MissesDeadline     bool `json:"missesDeadline,omitempty"`
}

// maxChangeLog bounds the history kept on a plan.
const maxChangeLog = 50

func appendChangeRecord(log []PlanChangeRecord, rec PlanChangeRecord) []PlanChangeRecord {
	log = append(log, rec)
	if len(log) > maxChangeLog {
		log = append([]PlanChangeRecord(nil), log[len(log)-maxChangeLog:]...)
	}
	return log
}

// normalizeChangeType maps whatever the model said onto the closed set.
func normalizeChangeType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "" || t == "none" {
		return changeNone
	}
	if knownChangeTypes[t] {
		return t
	}
	return changeOther
}

// availabilityFromChange validates the availability half of a request. It runs
// the model's numbers through exactly the same normalization as a typed answer,
// so a silly figure cannot enter through this door instead of the interview.
func availabilityFromChange(req planChangeRequest) StudyAvailability {
	var av StudyAvailability
	av.Days = append([]string(nil), req.Days...)
	switch {
	case req.WeeklyMinutes > 0:
		av.WeeklyMinutes = req.WeeklyMinutes
	case req.HoursPerWeek > 0:
		av.WeeklyMinutes = clamp(req.HoursPerWeek, 1, 40) * 60
	}
	for _, pd := range req.PerDay {
		if pd.Minutes > 0 {
			av.PerDay = append(av.PerDay, pd)
		}
	}
	av.normalize()
	return av
}

// currentAvailability reconstructs what the session actually knows, tolerating
// records written before Availability existed (and tests that set only the
// legacy mirrors). It never invents a default — an unknown half stays unknown,
// which is the whole point of the type.
func currentAvailability(sess *IntakeSession) StudyAvailability {
	av := sess.Answers.Availability.clone()
	if !av.HasDays() && len(sess.Answers.Days) > 0 {
		av.Days = append([]string(nil), sess.Answers.Days...)
	}
	if !av.HasTime() && sess.Answers.HoursPerWeek > 0 {
		av.WeeklyMinutes = sess.Answers.HoursPerWeek * 60
	}
	av.normalize()
	return av
}

// syncAnswersAvailability writes an availability onto the session, keeping the
// legacy mirrors in step so nothing downstream reads a stale day list.
func syncAnswersAvailability(sess *IntakeSession, av StudyAvailability) {
	av.normalize()
	sess.Answers.Availability = av
	sess.Answers.Days = append([]string(nil), av.Days...)
	sess.Answers.HoursPerWeek = av.HoursPerWeek()
}

// syncPlanAvailability writes an availability onto the plan, again keeping the
// published HoursPerWeek/Days fields in step with the authoritative minutes.
func syncPlanAvailability(plan *Plan, av StudyAvailability) {
	av.normalize()
	plan.Days = append([]string(nil), av.Days...)
	plan.HoursPerWeek = av.HoursPerWeek()
	plan.WeeklyMinutes = av.WeeklyMinutes
	plan.PerDay = append([]DayAvailability(nil), av.PerDay...)
}

// planAvailabilityOf reconstructs a plan's availability from its stored fields.
func planAvailabilityOf(plan *Plan) StudyAvailability {
	av := StudyAvailability{
		Days:          append([]string(nil), plan.Days...),
		WeeklyMinutes: plan.WeeklyMinutes,
		PerDay:        append([]DayAvailability(nil), plan.PerDay...),
	}
	if av.WeeklyMinutes <= 0 && plan.HoursPerWeek > 0 {
		av.WeeklyMinutes = plan.HoursPerWeek * 60
	}
	av.normalize()
	return av
}

// rescheduleLocked lays the plan out again and reports what moved.
//
// The caller MUST hold the plan's mutation lock. Completed and skipped sessions
// are preserved by Scheduler.Schedule itself — it only ever re-places
// outstanding work — so replanning can never discard finished progress.
func (p *Pipeline) rescheduleLocked(plan *Plan) (future, preserved int) {
	existing := p.store.EventsForPlan(plan.ID)
	for _, ev := range existing {
		if ev.Status == "done" || ev.Status == "skipped" {
			preserved++
		}
	}
	events := p.sched.Schedule(plan, existing)
	p.store.ReplaceEventsForPlan(plan.ID, events)
	return len(events) - preserved, preserved
}

// applyPlanChange validates one proposed change and applies it to the plan.
//
// It returns the record of what happened and whether anything actually changed.
// A type it does not recognize, or one whose payload does not validate, is a
// no-op: the user still gets an answer, the plan is simply untouched.
func (p *Pipeline) applyPlanChange(sess *IntakeSession, plan *Plan, req planChangeRequest) (PlanChangeRecord, bool) {
	rec := PlanChangeRecord{At: time.Now(), Type: normalizeChangeType(req.Type)}
	loc := loadLocation(plan.Timezone)

	switch rec.Type {
	case changeAvailability, changeWeeklyTime:
		return p.applyAvailabilityChange(sess, plan, req, rec, loc)
	case changeDeadline:
		return p.applyDeadlineChange(sess, plan, req, rec, loc)
	case changeSessionDuration:
		return p.applySessionDurationChange(plan, req, rec, loc)
	case changeResourceReplace:
		return p.applyResourceReplace(plan, req, rec, loc)
	case changeResourceRemove:
		return p.applyResourceRemove(plan, req, rec)
	case changeResourceAdd:
		if name := strings.TrimSpace(req.ResourceTo); name != "" {
			if addResource(plan, name, req.Reason, req.ResourceKind) != nil {
				rec.ResourceTo = name
				rec.Summary = "Added " + name + " to your resources."
				plan.ChangeLog = appendChangeRecord(plan.ChangeLog, rec)
				p.store.SavePlan(plan)
				return rec, true
			}
		}
	}
	// changeTarget is handled by the caller: it reopens the confirmation gate
	// rather than being applied here, because a new target needs a new plan and
	// the user has to agree to it first.
	return rec, false
}

func (p *Pipeline) applyAvailabilityChange(sess *IntakeSession, plan *Plan, req planChangeRequest, rec PlanChangeRecord, loc *time.Location) (PlanChangeRecord, bool) {
	current := planAvailabilityOf(plan)
	proposed := availabilityFromChange(req)
	if !proposed.HasDays() && !proposed.HasTime() {
		return rec, false
	}
	next := mergeAvailability(current, proposed)
	if !next.Complete() {
		// Half an availability cannot schedule anything; leave the plan alone
		// and let the assistant ask for the missing half.
		return rec, false
	}
	if availabilityFingerprint(next) == availabilityFingerprint(current) {
		// Idempotency: the same change applied twice is not a second change,
		// and must not produce a second reschedule or a second log entry.
		return rec, false
	}

	rec.OldDays = append([]string(nil), current.Days...)
	rec.NewDays = append([]string(nil), next.Days...)
	rec.OldWeeklyMinutes = current.WeeklyMinutes
	rec.NewWeeklyMinutes = next.WeeklyMinutes
	rec.OldFinishDate = plan.FinishDate

	syncAnswersAvailability(sess, next)
	syncPlanAvailability(plan, next)
	p.store.UpdateUserAvailability(sess.UserID, next.HoursPerWeek(), next.Days)

	rec.FutureSessions, rec.CompletedPreserved = p.rescheduleLocked(plan)
	rec.Rescheduled = true
	p.finishChange(plan, &rec, loc)
	return rec, true
}

func (p *Pipeline) applyDeadlineChange(sess *IntakeSession, plan *Plan, req planChangeRequest, rec PlanChangeRecord, loc *time.Location) (PlanChangeRecord, bool) {
	d := strings.TrimSpace(req.Deadline)
	if !validDate(d) || d == plan.Deadline {
		return rec, false
	}
	rec.OldDeadline = plan.Deadline
	rec.NewDeadline = d
	rec.OldFinishDate = plan.FinishDate

	plan.Deadline = d
	sess.Answers.Deadline = d
	rec.FutureSessions, rec.CompletedPreserved = p.rescheduleLocked(plan)
	rec.Rescheduled = true
	p.finishChange(plan, &rec, loc)
	return rec, true
}

func (p *Pipeline) applySessionDurationChange(plan *Plan, req planChangeRequest, rec PlanChangeRecord, loc *time.Location) (PlanChangeRecord, bool) {
	cap := req.SessionMaxMinutes
	if cap <= 0 {
		cap = req.SessionMinutes
	}
	if cap < minDayMinutes || cap > maxDayMinutes {
		return rec, false
	}
	rec.OldFinishDate = plan.FinishDate
	touched := 0
	for i := range plan.Todos {
		t := &plan.Todos[i]
		if t.DurationMin > cap {
			t.DurationMin = cap
			touched++
		}
	}
	if touched == 0 {
		return rec, false
	}
	rec.Summary = "Capped sessions at " + itoa(cap) + " minutes."
	rec.FutureSessions, rec.CompletedPreserved = p.rescheduleLocked(plan)
	rec.Rescheduled = true
	p.finishChange(plan, &rec, loc)
	return rec, true
}

func (p *Pipeline) applyResourceReplace(plan *Plan, req planChangeRequest, rec PlanChangeRecord, loc *time.Location) (PlanChangeRecord, bool) {
	to := strings.TrimSpace(req.ResourceTo)
	if to == "" {
		return rec, false
	}
	sel := findResource(plan, req.ResourceFrom)
	if sel == nil {
		// Nothing recognizable to replace: record it as an addition instead of
		// rewriting a resource we may have mis-identified.
		if addResource(plan, to, req.Reason, req.ResourceKind) == nil {
			return rec, false
		}
		rec.Type = changeResourceAdd
		rec.ResourceTo = to
		rec.Summary = "Added " + to + " to your resources."
		plan.ChangeLog = appendChangeRecord(plan.ChangeLog, rec)
		p.store.SavePlan(plan)
		return rec, true
	}

	from := sel.InUse()
	if strings.EqualFold(from, to) {
		return rec, false // already using it; idempotent
	}
	rec.ResourceFrom = from
	rec.ResourceTo = to
	rec.OldFinishDate = plan.FinishDate
	replaceResource(plan, sel, to, req.Equivalent, req.Reason)

	// MINIMAL CHANGE. An equivalent swap is a relabelling: the schedule is
	// already right, so it stays exactly as it is. Only a workload that
	// genuinely differs earns a touch of the learning content, and then only
	// the todos that actually used that resource.
	if !req.WorkloadChanged {
		rec.Summary = "Now using " + to + " instead of " + from + "."
		plan.ChangeLog = appendChangeRecord(plan.ChangeLog, rec)
		p.store.SavePlan(plan)
		return rec, true
	}

	touched := 0
	for i := range plan.Todos {
		t := &plan.Todos[i]
		if !strings.EqualFold(t.ResourceRef, to) {
			continue
		}
		if req.SessionMinutes >= minDayMinutes && req.SessionMinutes <= maxDayMinutes {
			t.DurationMin = req.SessionMinutes
			touched++
		}
		if f := normalizeFrequency(req.Frequency); strings.TrimSpace(req.Frequency) != "" && f != t.Frequency {
			t.Frequency = f
			touched++
		}
	}
	rec.Summary = "Switched to " + to + " and adjusted the work that used " + from + "."
	rec.FutureSessions, rec.CompletedPreserved = p.rescheduleLocked(plan)
	rec.Rescheduled = true
	p.finishChange(plan, &rec, loc)
	_ = touched
	return rec, true
}

func (p *Pipeline) applyResourceRemove(plan *Plan, req planChangeRequest, rec PlanChangeRecord) (PlanChangeRecord, bool) {
	sel := findResource(plan, firstNonEmpty(req.ResourceFrom, req.ResourceTo))
	if sel == nil {
		return rec, false
	}
	rec.ResourceFrom = sel.InUse()
	removeResource(plan, sel)
	rec.Summary = "Dropped " + rec.ResourceFrom + " from your plan."
	plan.ChangeLog = appendChangeRecord(plan.ChangeLog, rec)
	p.store.SavePlan(plan)
	return rec, true
}

// finishChange completes a rescheduling change: milestones travel with the
// finish date, the deadline verdict is recomputed, and the record is logged and
// persisted. Leaving milestones pinned to their old dates would expire them the
// moment anything moved.
func (p *Pipeline) finishChange(plan *Plan, rec *PlanChangeRecord, loc *time.Location) {
	rec.NewFinishDate = plan.FinishDate
	if o, ok := parseDateIn(rec.OldFinishDate, loc); ok {
		if n, ok2 := parseDateIn(plan.FinishDate, loc); ok2 {
			rec.FinishShift = daysBetween(o, n)
		}
	}
	if rec.FinishShift != 0 {
		shiftMilestones(plan, dateStr(todayIn(loc)), rec.FinishShift, loc)
	}
	applyDeadlineCheck(plan, loc)
	rec.MissesDeadline = plan.MissesDeadline
	if rec.Summary == "" {
		rec.Summary = changeSummary(*rec)
	}
	plan.ChangeLog = appendChangeRecord(plan.ChangeLog, *rec)
	p.store.SavePlan(plan)
}

// changeSummary renders a factual one-liner for the change log.
func changeSummary(rec PlanChangeRecord) string {
	parts := []string{}
	if len(rec.NewDays) > 0 && strings.Join(rec.OldDays, ",") != strings.Join(rec.NewDays, ",") {
		parts = append(parts, "days "+strings.Join(rec.OldDays, "/")+" → "+strings.Join(rec.NewDays, "/"))
	}
	if rec.NewWeeklyMinutes > 0 && rec.NewWeeklyMinutes != rec.OldWeeklyMinutes {
		parts = append(parts, "weekly time "+itoa(rec.OldWeeklyMinutes)+"m → "+itoa(rec.NewWeeklyMinutes)+"m")
	}
	if rec.NewDeadline != "" && rec.NewDeadline != rec.OldDeadline {
		parts = append(parts, "deadline "+rec.OldDeadline+" → "+rec.NewDeadline)
	}
	if rec.FinishShift != 0 {
		parts = append(parts, "finish "+rec.OldFinishDate+" → "+rec.NewFinishDate)
	}
	if len(parts) == 0 {
		return rec.Type
	}
	return strings.Join(parts, "; ")
}

// changeEffectLine is the plain explanation shown to the user after a change.
// The wording is the backend's, not the model's, so the numbers in it are the
// numbers that were actually applied.
func changeEffectLine(lang string, rec PlanChangeRecord) string {
	if !rec.Rescheduled {
		return ""
	}
	days := strings.Join(rec.NewDays, ", ")
	hours := itoa(maxInt(1, (rec.NewWeeklyMinutes+30)/60))
	var sb strings.Builder
	switch {
	case len(rec.NewDays) > 0 && rec.NewWeeklyMinutes != rec.OldWeeklyMinutes:
		sb.WriteString(tr(lang,
			"Updated: "+days+", about "+hours+"h a week.",
			"Обновлено: "+days+", около "+hours+" ч в неделю.",
			"Yangilandi: "+days+", haftasiga taxminan "+hours+" soat."))
	case len(rec.NewDays) > 0:
		sb.WriteString(tr(lang,
			"Updated your study days to "+days+".",
			"Ваши учебные дни обновлены: "+days+".",
			"O'quv kunlaringiz yangilandi: "+days+"."))
	case rec.NewWeeklyMinutes > 0:
		sb.WriteString(tr(lang,
			"Updated your weekly time to about "+hours+" hours.",
			"Ваше недельное время обновлено: около "+hours+" ч.",
			"Haftalik vaqtingiz yangilandi: taxminan "+hours+" soat."))
	case rec.NewDeadline != "":
		sb.WriteString(tr(lang,
			"Moved your deadline to "+rec.NewDeadline+".",
			"Срок перенесён на "+rec.NewDeadline+".",
			"Muddat "+rec.NewDeadline+" ga ko'chirildi."))
	}
	sb.WriteString(tr(lang,
		" I rescheduled "+itoa(rec.FutureSessions)+" upcoming session(s); "+itoa(rec.CompletedPreserved)+" completed one(s) stayed as they were.",
		" Я перепланировал "+itoa(rec.FutureSessions)+" предстоящих занятий; "+itoa(rec.CompletedPreserved)+" завершённых остались без изменений.",
		" Men "+itoa(rec.FutureSessions)+" ta bo'lajak mashg'ulotni qayta rejalashtirdim; "+itoa(rec.CompletedPreserved)+" ta tugallangani o'z holicha qoldi."))
	switch {
	case rec.FinishShift > 0:
		sb.WriteString(tr(lang,
			" Your finish date moves from "+rec.OldFinishDate+" to "+rec.NewFinishDate+".",
			" Дата завершения сдвигается с "+rec.OldFinishDate+" на "+rec.NewFinishDate+".",
			" Tugash sanangiz "+rec.OldFinishDate+" dan "+rec.NewFinishDate+" ga suriladi."))
	case rec.FinishShift < 0:
		sb.WriteString(tr(lang,
			" Your finish date moves earlier, from "+rec.OldFinishDate+" to "+rec.NewFinishDate+".",
			" Дата завершения сдвигается раньше: с "+rec.OldFinishDate+" на "+rec.NewFinishDate+".",
			" Tugash sanangiz oldinga suriladi: "+rec.OldFinishDate+" dan "+rec.NewFinishDate+" ga."))
	}
	return strings.TrimSpace(sb.String())
}
