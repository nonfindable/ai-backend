package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Scheduler places sized todos onto real dates and rolls missed days forward.
// This is deliberately plain, deterministic code — NOT the model's job.
//
// The placement pass honours three things the plan actually specifies and that
// an earlier version ignored: the user's weekly time budget, each todo's stated
// cadence (twice_weekly means twice per week, not twice in one day), and each
// phase's week window (rehearsal work does not start in week one).
type Scheduler struct {
	store *Store

	// planLocks is THE mutation boundary for a plan and its calendar. Every
	// writer — the schedule, confirm and complete handlers, the rollover
	// endpoint and the nightly sweep — serialises on it. It used to be split
	// across a per-plan lock in the API and a per-user lock for rollover, which
	// are different mutexes and therefore excluded nothing: a rollover holding a
	// stale event list could write it back after a reschedule had replaced it,
	// resurrecting every deleted event.
	//
	// The API shares this instance (see newAPI) rather than making its own.
	// Lock order, where both are taken, is always user then plan.
	planLocks *keyedMutex
}

func newScheduler(store *Store) *Scheduler {
	return &Scheduler{store: store, planLocks: newKeyedMutex()}
}

// eventID derives a calendar event's identity from the session it represents
// rather than from a random draw. Two runs of the scheduler over the same plan
// therefore produce the same IDs, which makes rescheduling idempotent and lets
// the store reject a duplicate instead of storing it twice under two names.
func eventID(planID, todoID string, occurrence int) string {
	sum := sha256.Sum256([]byte(planID + "\x00" + todoID + "\x00" + itoa(occurrence)))
	return "evt_" + hex.EncodeToString(sum[:8])
}

const (
	// maxSessionsPerDay only stops a day being fragmented into many tiny
	// blocks; the real limits are the per-day and per-week minute budgets
	// derived from the user's stated hours. A hard cap of 2 silently became the
	// binding constraint and pushed work outside its own phase window.
	maxSessionsPerDay = 4
	dayStartMinute    = 18 * 60 // 18:00
	slotGapMinute     = 15
	defaultHoursWeek  = 6
	maxPlanWeeks      = 52

	// overflowWeeks bounds how far past the plan's nominal end a session may
	// spill when its own phase window is full or already in the past.
	overflowWeeks = 8
)

// cadencePerWeek converts a frequency into sessions per week.
func cadencePerWeek(freq string, availableDays int) int {
	switch freq {
	case "once":
		return 1
	case "weekly":
		return 1
	case "twice_weekly":
		return 2
	case "thrice_weekly":
		return 3
	case "daily":
		return maxInt(1, availableDays)
	default:
		return 1
	}
}

func priorityRank(p string) int {
	switch strings.ToLower(p) {
	case "high":
		return 0
	case "low":
		return 2
	default:
		return 1
	}
}

// phaseWindow returns the 0-based, inclusive week range for a todo's phase.
// Todos with no or an unknown phase may use the whole plan.
func phaseWindow(plan *Plan, phaseKey string, weeks int) (int, int) {
	for _, ph := range plan.Phases {
		if ph.Key != phaseKey || phaseKey == "" {
			continue
		}
		start := clamp(ph.WeekStart, 1, weeks) - 1
		end := clamp(ph.WeekEnd, 1, weeks) - 1
		if end < start {
			end = start
		}
		return start, end
	}
	return 0, weeks - 1
}

// daySlot tracks what is already committed on one calendar day.
type daySlot struct {
	count     int
	minutes   int
	nextStart int
}

// demand is one session of one todo that still needs a date.
type demand struct {
	todoIdx  int
	week     int // earliest week it may land in (0-based)
	maxWeek  int // last week of its phase window
	seq      int // occurrence index within the week, for round-robin fairness
	occ      int // occurrence index within the todo's whole series, for eventID
	duration int
	priority int
}

// Schedule places every outstanding session of a plan's todos and returns the
// full event set, preserving sessions that are already done or skipped.
//
// The caller MUST hold sc.planLocks for plan.ID across the read-modify-write it
// performs (load plan + events, Schedule, store the result). Schedule does not
// take the lock itself because its callers need the whole sequence to be
// atomic, and keyedMutex is not reentrant.
func (sc *Scheduler) Schedule(plan *Plan, existing []*CalendarEvent) []*CalendarEvent {
	loc := loadLocation(plan.Timezone)
	dayset := parseWeekdaySet(plan.Days)
	weeks := clamp(plan.WeeksTotal, 1, maxPlanWeeks)

	// The anchor is the plan's week-0 origin and stays fixed across reschedules
	// so week numbering (and therefore phase windows) does not drift. Only the
	// earliest placeable date moves forward with the calendar.
	anchor, ok := parseDateIn(plan.StartDate, loc)
	if !ok {
		anchor = todayIn(loc).AddDate(0, 0, 1)
	}
	earliest := todayIn(loc).AddDate(0, 0, 1)
	if anchor.After(earliest) {
		earliest = anchor
	}

	weeklyBudget := plan.HoursPerWeek * 60
	if weeklyBudget <= 0 {
		weeklyBudget = defaultHoursWeek * 60
	}

	// Keep finished history exactly where it is; only outstanding work moves.
	var kept []*CalendarEvent
	slots := map[string]*daySlot{}
	completed := map[string]int{}
	keptIDs := map[string]bool{}
	for _, ev := range existing {
		if ev.Status != "done" && ev.Status != "skipped" {
			continue
		}
		k := ev.clone()
		kept = append(kept, k)
		keptIDs[k.ID] = true
		if k.Status == "done" {
			completed[k.TodoID]++
		}
		s := slotFor(slots, k.Date)
		s.count++
		s.minutes += k.DurationMin
		if end := startMinuteOf(k.StartTime) + k.DurationMin + slotGapMinute; end > s.nextStart {
			s.nextStart = end
		}
	}

	// Expand each todo into per-week demands at its stated cadence, inside its
	// phase window, minus whatever is already done.
	var demands []demand
	availableDayCount := len(dayset)
	for i := range plan.Todos {
		t := &plan.Todos[i]
		if t.Status == "skipped" {
			t.PlannedCount = 0
			continue
		}
		w0, w1 := phaseWindow(plan, t.Phase, weeks)
		dur := t.DurationMin
		if dur <= 0 {
			dur = 45
		}
		var own []demand
		if t.Frequency == "once" {
			own = append(own, demand{todoIdx: i, week: w0, maxWeek: w1, seq: 0, occ: 0, duration: dur, priority: priorityRank(t.Priority)})
		} else {
			per := cadencePerWeek(t.Frequency, availableDayCount)
			for w := w0; w <= w1; w++ {
				for k := 0; k < per; k++ {
					own = append(own, demand{todoIdx: i, week: w, maxWeek: w1, seq: k, occ: len(own), duration: dur, priority: priorityRank(t.Priority)})
				}
			}
		}
		t.PlannedCount = len(own)
		t.CompletedCount = minInt(completed[t.ID], len(own))

		// Drop the occurrences a finished event already covers, matching on the
		// event's derived identity rather than on how many are done. Slicing the
		// first CompletedCount off the front assumed completions arrive in order,
		// so completing a later session by eventId regenerated an occurrence that
		// a kept "done" event still owned — two events, one ID.
		outstanding := own[:0]
		for _, d := range own {
			if keptIDs[eventID(plan.ID, t.ID, d.occ)] {
				continue
			}
			outstanding = append(outstanding, d)
		}
		own = outstanding

		syncTodoStatus(t)
		demands = append(demands, own...)
	}

	// Order globally by week, then by occurrence index so every todo gets its
	// first session of the week before any todo gets its second, then by
	// priority. This is what stops one todo from consuming the whole budget.
	sort.SliceStable(demands, func(a, b int) bool {
		x, y := demands[a], demands[b]
		if x.week != y.week {
			return x.week < y.week
		}
		if x.seq != y.seq {
			return x.seq < y.seq
		}
		if x.priority != y.priority {
			return x.priority < y.priority
		}
		return x.todoIdx < y.todoIdx
	})

	weekDates := memoWeekDates(anchor, earliest, dayset)
	weekLeft := map[int]int{}
	budgetFor := func(w int) int {
		if v, ok := weekLeft[w]; ok {
			return v
		}
		weekLeft[w] = weeklyBudget
		return weeklyBudget
	}

	var events []*CalendarEvent
	dropped := 0
	tryPlace := func(dm demand, t *Todo, from, to int) bool {
		for w := from; w <= to; w++ {
			if w < 0 || budgetFor(w) < dm.duration {
				continue
			}
			dates := weekDates(w)
			if len(dates) == 0 {
				continue
			}
			limit := dayMinuteCap(weeklyBudget, len(dates), dm.duration)
			for _, d := range dates {
				ds := dateStr(d)
				s := slotFor(slots, ds)
				if s.count >= maxSessionsPerDay {
					continue
				}
				if s.minutes+dm.duration > limit {
					continue
				}
				events = append(events, &CalendarEvent{
					ID:           eventID(plan.ID, t.ID, dm.occ),
					UserID:       plan.UserID,
					PlanID:       plan.ID,
					TodoID:       t.ID,
					Title:        t.Title,
					Date:         ds,
					StartTime:    formatMinute(s.nextStart),
					DurationMin:  dm.duration,
					ReminderMin:  30,
					Status:       "proposed",
					ExportTarget: "web_calendar",
				})
				s.count++
				s.minutes += dm.duration
				s.nextStart += dm.duration + slotGapMinute
				weekLeft[w] = budgetFor(w) - dm.duration
				return true
			}
		}
		return false
	}

	for _, dm := range demands {
		t := &plan.Todos[dm.todoIdx]
		// Prefer the todo's own phase window, but never delete outstanding work
		// just because that window has already gone by — which is exactly what
		// happens when an in-progress plan is rescheduled.
		placed := tryPlace(dm, t, dm.week, dm.maxWeek)
		if !placed {
			placed = tryPlace(dm, t, dm.maxWeek+1, weeks-1+overflowWeeks)
		}
		if !placed {
			dropped++
			// The series is shorter than intended; keep PlannedCount honest so
			// progress percentages are not computed against sessions that were
			// never scheduled.
			if t.PlannedCount > 0 {
				t.PlannedCount--
			}
		}
	}

	for _, t := range kept {
		events = append(events, t)
	}
	sortEvents(events)

	plan.StartDate = dateStr(anchor)
	if len(events) > 0 {
		plan.FinishDate = events[len(events)-1].Date
	} else {
		plan.FinishDate = plan.StartDate
	}
	if plan.OriginalFinishDate == "" {
		plan.OriginalFinishDate = plan.FinishDate
	}
	plan.DroppedSessions = dropped
	for i := range plan.Todos {
		syncTodoStatus(&plan.Todos[i])
	}
	applyDeadlineCheck(plan, loc)
	return events
}

// dayMinuteCap allows a day to run up to 1.5x the week's average so a single
// long session still fits, but never less than the session being placed.
func dayMinuteCap(weeklyBudget, daysInWeek, need int) int {
	if daysInWeek <= 0 {
		return need
	}
	perDay := (weeklyBudget * 3) / (2 * daysInWeek)
	return maxInt(perDay, need)
}

func slotFor(slots map[string]*daySlot, date string) *daySlot {
	if s, ok := slots[date]; ok {
		return s
	}
	s := &daySlot{nextStart: dayStartMinute}
	slots[date] = s
	return s
}

// memoWeekDates returns the placeable dates of week w, computed once per week.
func memoWeekDates(anchor, earliest time.Time, dayset map[time.Weekday]bool) func(int) []time.Time {
	cache := map[int][]time.Time{}
	return func(w int) []time.Time {
		if v, ok := cache[w]; ok {
			return v
		}
		var out []time.Time
		for i := 0; i < 7; i++ {
			d := anchor.AddDate(0, 0, w*7+i)
			if d.Before(earliest) {
				continue
			}
			if dayset[d.Weekday()] {
				out = append(out, d)
			}
		}
		cache[w] = out
		return out
	}
}

func formatMinute(m int) string {
	m = clamp(m, 0, 23*60+59)
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

func startMinuteOf(hhmm string) int {
	if len(hhmm) != 5 || hhmm[2] != ':' {
		return dayStartMinute
	}
	h, okh := parseInt(hhmm[:2])
	m, okm := parseInt(hhmm[3:])
	if !okh || !okm {
		return dayStartMinute
	}
	return clamp(h, 0, 23)*60 + clamp(m, 0, 59)
}

// syncTodoStatus derives a todo's status from its per-occurrence counters.
func syncTodoStatus(t *Todo) {
	if t.Status == "skipped" {
		return
	}
	switch {
	case t.PlannedCount > 0 && t.CompletedCount >= t.PlannedCount:
		t.Status = "done"
	case t.CompletedCount > 0:
		t.Status = "in_progress"
	default:
		t.Status = "pending"
		t.CompletedAt = nil
	}
}

// applyDeadlineCheck compares the schedule against the user's stated deadline.
// The deadline used to only pick a plan length; nothing ever checked whether
// the resulting schedule actually met it.
func applyDeadlineCheck(plan *Plan, loc *time.Location) {
	plan.MissesDeadline = false
	plan.DeadlineSlipDays = 0
	plan.DeadlineNote = ""
	dl, ok := parseDateIn(plan.Deadline, loc)
	if !ok {
		return
	}
	fin, ok2 := parseDateIn(plan.FinishDate, loc)
	if !ok2 {
		return
	}
	slip := daysBetween(dl, fin)
	if slip <= 0 {
		return
	}
	plan.MissesDeadline = true
	plan.DeadlineSlipDays = slip
	plan.DeadlineNote = tr(plan.Lang,
		fmt.Sprintf("This schedule finishes %s, which is %d day(s) after your %s deadline. Add hours per week or trim the target to close the gap.", plan.FinishDate, slip, plan.Deadline),
		fmt.Sprintf("Расписание завершается %s — это на %d дн. позже вашего срока %s. Добавьте часов в неделю или сократите цель, чтобы закрыть разрыв.", plan.FinishDate, slip, plan.Deadline),
		fmt.Sprintf("Jadval %s da tugaydi — bu %s muddatidan %d kun kech. Farqni yopish uchun haftalik soatni oshiring yoki maqsadni qisqartiring.", plan.FinishDate, plan.Deadline, slip))
}

// RolloverSummary reports what a rollover run changed.
type RolloverSummary struct {
	PlanID         string `json:"planId"`
	Moved          int    `json:"moved"`
	OldFinish      string `json:"oldFinish"`
	NewFinish      string `json:"newFinish"`
	FinishShifts   int    `json:"finishShiftDays"`
	MissesDeadline bool   `json:"missesDeadline"`
	DeadlineNote   string `json:"deadlineNote,omitempty"`
	Message        string `json:"message"`
}

// Rollover moves every past, still-pending session forward to the next available
// day and recomputes the finish date — the plan "procrastinates" with the user.
func (sc *Scheduler) Rollover(userID string, ref time.Time) []RolloverSummary {
	summaries := []RolloverSummary{}
	// PlansByUser is only used to enumerate IDs; each plan is re-read inside its
	// own lock, because this snapshot is stale the moment it is returned.
	for _, p := range sc.store.PlansByUser(userID) {
		if sum, ok := sc.rolloverPlan(p.ID, userID, ref); ok {
			summaries = append(summaries, sum)
		}
	}
	return summaries
}

// rolloverPlan rolls one plan forward while holding that plan's mutation lock,
// so it cannot interleave with a reschedule, a confirm or a completion.
func (sc *Scheduler) rolloverPlan(planID, userID string, ref time.Time) (RolloverSummary, bool) {
	unlock := sc.planLocks.Lock(planID)
	defer unlock()

	plan, ok := sc.store.GetPlan(planID)
	if !ok || plan.UserID != userID {
		return RolloverSummary{}, false
	}
	{
		loc := loadLocation(plan.Timezone)
		refStr := dateStr(ref)
		events := sc.store.EventsForPlan(plan.ID)
		if len(events) == 0 {
			return RolloverSummary{}, false
		}

		dayset := parseWeekdaySet(plan.Days)
		weeklyBudget := plan.HoursPerWeek * 60
		if weeklyBudget <= 0 {
			weeklyBudget = defaultHoursWeek * 60
		}

		slots := map[string]*daySlot{}
		var missed []*CalendarEvent
		for _, ev := range events {
			pending := ev.Status != "done" && ev.Status != "skipped"
			if ev.Date < refStr && pending {
				missed = append(missed, ev)
				continue
			}
			s := slotFor(slots, ev.Date)
			s.count++
			s.minutes += ev.DurationMin
			if end := startMinuteOf(ev.StartTime) + ev.DurationMin + slotGapMinute; end > s.nextStart {
				s.nextStart = end
			}
		}
		if len(missed) == 0 {
			return RolloverSummary{}, false
		}

		oldFinish := plan.FinishDate
		dayCap := dayMinuteCap(weeklyBudget, maxInt(1, len(dayset)), 0)

		cursor := ref
		for _, ev := range missed {
			limit := maxInt(dayCap, ev.DurationMin)
			placed := false
			for guard := 0; guard < 3000 && !placed; guard++ {
				ds := dateStr(cursor)
				if dayset[cursor.Weekday()] {
					s := slotFor(slots, ds)
					if s.count < maxSessionsPerDay && s.minutes+ev.DurationMin <= limit {
						ev.Date = ds
						ev.StartTime = formatMinute(s.nextStart)
						ev.RolledOver++
						// Status is deliberately preserved: a session the user
						// never confirmed must not become "scheduled" just
						// because it slipped.
						s.count++
						s.minutes += ev.DurationMin
						s.nextStart += ev.DurationMin + slotGapMinute
						placed = true
						break
					}
				}
				cursor = cursor.AddDate(0, 0, 1)
			}
			if !placed {
				ev.Date = dateStr(cursor)
				ev.RolledOver++
			}
			sc.store.AddProgress(&ProgressLog{
				ID: newID("log"), UserID: userID, PlanID: plan.ID, TodoID: ev.TodoID,
				Event: "rolled_over", At: time.Now(),
				Note: "missed session rolled forward to " + ev.Date,
			})
		}
		sc.store.SaveEvents(events)

		newFinish := plan.StartDate
		for _, ev := range events {
			if ev.Date > newFinish {
				newFinish = ev.Date
			}
		}
		plan.FinishDate = newFinish

		shift := 0
		if o, ok := parseDateIn(oldFinish, loc); ok {
			if n, ok2 := parseDateIn(newFinish, loc); ok2 {
				shift = daysBetween(o, n)
			}
		}
		// Milestones are checkpoints on this timeline, so they have to travel
		// with it; leaving them pinned to the original dates made them expire
		// the moment anything slipped.
		if shift > 0 {
			shiftMilestones(plan, refStr, shift, loc)
		}
		applyDeadlineCheck(plan, loc)
		sc.store.SavePlan(plan)

		msg := fmt.Sprintf("%d session(s) rolled forward.", len(missed))
		if shift > 0 {
			msg += fmt.Sprintf(" Finish moved from %s → %s (+%d days).", oldFinish, newFinish, shift)
		}
		if plan.MissesDeadline {
			msg += " " + plan.DeadlineNote
		}
		return RolloverSummary{
			PlanID: plan.ID, Moved: len(missed), OldFinish: oldFinish,
			NewFinish: newFinish, FinishShifts: shift,
			MissesDeadline: plan.MissesDeadline, DeadlineNote: plan.DeadlineNote,
			Message: msg,
		}, true
	}
}

// shiftMilestones moves not-yet-reached milestones forward by days.
func shiftMilestones(plan *Plan, refStr string, days int, loc *time.Location) {
	for i := range plan.Milestones {
		m := &plan.Milestones[i]
		if m.Done || m.TargetDate < refStr {
			continue
		}
		if d, ok := parseDateIn(m.TargetDate, loc); ok {
			m.TargetDate = dateStr(d.AddDate(0, 0, days))
		}
	}
}

// ---- iCalendar export ----

// ICS renders a plan's events as an iCalendar file (used by the .ics export and
// as the interchange format for the web calendar). Times are emitted in UTC so
// they are unambiguous regardless of where the server runs, and every line is
// folded to the 75-octet limit RFC 5545 requires.
func (sc *Scheduler) ICS(plan *Plan, events []*CalendarEvent) string {
	loc := loadLocation(plan.Timezone)
	stamp := time.Now().UTC().Format("20060102T150405Z")

	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	icsLine(&b, "VERSION", "2.0")
	icsLine(&b, "PRODID", "-//start.ai//web//EN")
	icsLine(&b, "CALSCALE", "GREGORIAN")
	icsLine(&b, "METHOD", "PUBLISH")
	icsLine(&b, "X-WR-CALNAME", icsEscape("start.ai — "+plan.Skill))

	for _, ev := range events {
		d, ok := parseDateIn(ev.Date, loc)
		if !ok {
			continue
		}
		startMin := startMinuteOf(ev.StartTime)
		startT := time.Date(d.Year(), d.Month(), d.Day(), startMin/60, startMin%60, 0, 0, loc)
		dur := ev.DurationMin
		if dur <= 0 {
			dur = 45
		}
		endT := startT.Add(time.Duration(dur) * time.Minute)

		b.WriteString("BEGIN:VEVENT\r\n")
		icsLine(&b, "UID", ev.ID+"@start.ai")
		// DTSTAMP is mandatory in every VEVENT; without it strict parsers
		// reject the calendar outright.
		icsLine(&b, "DTSTAMP", stamp)
		icsLine(&b, "DTSTART", startT.UTC().Format("20060102T150405Z"))
		icsLine(&b, "DTEND", endT.UTC().Format("20060102T150405Z"))
		icsLine(&b, "SUMMARY", icsEscape(ev.Title))
		icsLine(&b, "DESCRIPTION", icsEscape("start.ai — "+plan.Skill))
		icsLine(&b, "STATUS", icsEventStatus(ev.Status))
		if ev.ReminderMin > 0 {
			b.WriteString("BEGIN:VALARM\r\n")
			icsLine(&b, "TRIGGER", "-PT"+itoa(ev.ReminderMin)+"M")
			icsLine(&b, "ACTION", "DISPLAY")
			icsLine(&b, "DESCRIPTION", icsEscape(ev.Title))
			b.WriteString("END:VALARM\r\n")
		}
		b.WriteString("END:VEVENT\r\n")
	}
	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

func icsEventStatus(s string) string {
	switch s {
	case "done":
		return "COMPLETED"
	case "skipped":
		return "CANCELLED"
	case "scheduled":
		return "CONFIRMED"
	default:
		return "TENTATIVE"
	}
}

// icsLine writes one folded "NAME:value" content line terminated by CRLF.
func icsLine(b *strings.Builder, name, value string) {
	b.WriteString(icsFold(name + ":" + value))
	b.WriteString("\r\n")
}

// icsFold breaks a content line at 75 octets, continuing with CRLF + a single
// space. It never splits a multi-byte character, which matters because plan
// titles arrive in Russian and Uzbek.
func icsFold(line string) string {
	const limit = 75
	if len(line) <= limit {
		return line
	}
	var b strings.Builder
	used := 0
	for i, w := 0, 0; i < len(line); i += w {
		_, w = utf8.DecodeRuneInString(line[i:])
		if used+w > limit {
			b.WriteString("\r\n ")
			used = 1 // the leading space counts toward the next line's limit
		}
		b.WriteString(line[i : i+w])
		used += w
	}
	return b.String()
}

func icsEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, ";", "\\;")
	s = strings.ReplaceAll(s, ",", "\\,")
	s = strings.ReplaceAll(s, "\r\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\n")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}
