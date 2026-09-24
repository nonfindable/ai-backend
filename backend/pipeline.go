package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---- stage result shapes (shared by the real-AI and mock paths) ----

type understandResult struct {
	InScope                bool     `json:"inScope"`
	Decline                string   `json:"decline"`
	Skill                  string   `json:"skill"`
	NeedsDisambiguation    bool     `json:"needsDisambiguation"`
	DisambiguationQuestion string   `json:"disambiguationQuestion"`
	Options                []string `json:"options"`
	PivotalChoice          string   `json:"pivotalChoice"`
	Overview               string   `json:"overview"`
}

type intakeResult struct {
	Answers      answerMap `json:"answers"`
	NextQuestion string    `json:"nextQuestion"`
	Options      []string  `json:"options"`
	// Asked is the category the question actually covers. The backend names the
	// category to ask about, but a reply that volunteers extra detail lets the
	// model skip ahead, and this is how it says so.
	Asked string `json:"asked"`

	// A user mid-interview does not only answer: they ask ("do I need a tutor?",
	// "is Anki any good?") and they request ("please include speaking practice").
	// Treating either as the answer to the pending question filed nonsense as
	// their level or their budget and lost what they actually said.
	LatestWasQuestion bool   `json:"latestWasQuestion"`
	ReplyToUser       string `json:"replyToUser"`
	NoteForPlan       string `json:"noteForPlan"`
	// Done is decoded but overwritten: nextIntakeCategory decides when the
	// interview ends, so a model that wants to stop early cannot skip the
	// questions a plan depends on.
	Done bool `json:"done"`
}

// answerMap decodes the intake prompt's "answers" object leniently. The prompt
// asks for mixed value types (hoursPerWeek as a number, days as an array of
// strings, the rest as strings), but every consumer downstream — applyAnswers,
// parseHoursPerWeek, detectDays — works on strings. Rather than trust each model
// to quote its numbers (some do, some don't, which is exactly what broke live
// intake on stricter models), we coerce every value to a string here. The
// underlying type stays map[string]string, so applyAnswers and the mock builder
// use it unchanged.
type answerMap map[string]string

func (a *answerMap) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	m := make(map[string]string, len(raw))
	for k, v := range raw {
		m[k] = coerceScalar(v)
	}
	*a = m
	return nil
}

// coerceScalar renders one JSON value as the plain string the intake consumers
// expect: a string is unquoted, a number/bool becomes its literal token, an
// array is comma-joined (so ["Mon","Tue"] feeds detectDays as "Mon,Tue"), and
// null or an object collapses to "" since there is nothing to parse from them.
func coerceScalar(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	switch s[0] {
	case '"':
		var str string
		if json.Unmarshal(raw, &str) == nil {
			return str
		}
		return ""
	case '[':
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) != nil {
			return ""
		}
		parts := make([]string, 0, len(arr))
		for _, el := range arr {
			if p := coerceScalar(el); p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, ",")
	case '{':
		return ""
	default: // number, true, false
		return s
	}
}

type phaseAI struct {
	Key       string `json:"key"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	WeekStart int    `json:"weekStart"`
	WeekEnd   int    `json:"weekEnd"`
}
type milestoneAI struct {
	Title      string `json:"title"`
	Phase      string `json:"phase"`
	TargetWeek int    `json:"targetWeek"`
}
type todoAI struct {
	Title       string `json:"title"`
	DurationMin int    `json:"durationMin"`
	Frequency   string `json:"frequency"`
	Priority    string `json:"priority"`
	Phase       string `json:"phase"`
	// DependsOn is still decoded for compatibility, but the plan prompt no
	// longer asks for it: nothing schedules on dependencies (ordering comes
	// from the phase windows), so asking only gave the model one more field to
	// get wrong. ResourceRef is asked for, and now has a definition the model
	// can follow instead of an empty string in an example.
	DependsOn   []string `json:"dependsOn"`
	ResourceRef string   `json:"resourceRef"`
}
type setupAI struct {
	Name       string `json:"name"`
	Category   string `json:"category"`
	Priority   string `json:"priority"`
	PriceRange string `json:"priceRange"`
	Owned      bool   `json:"owned"`
	Rationale  string `json:"rationale"`
	// SearchQuery is optional: when empty, shop links search by Name.
	SearchQuery string `json:"searchQuery"`
}
type planAI struct {
	Assessment  string        `json:"assessment"`
	Feasibility string        `json:"feasibility"`
	WeeksTotal  int           `json:"weeksTotal"`
	Phases      []phaseAI     `json:"phases"`
	Milestones  []milestoneAI `json:"milestones"`
	Todos       []todoAI      `json:"todos"`
	SetupItems  []setupAI     `json:"setupItems"`
}

// Turn is one assistant response to the client.
//
// Stage is the authoritative state; a frontend must switch on it rather than
// parsing Assistant. The stages are:
//
//	scope_check     the goal has not been accepted yet (also the opening state)
//	out_of_scope    the message was not a learning goal; Assistant is the redirect
//	disambiguation  one narrowing question, with Options as quick replies
//	intake          an adaptive interview question, with Progress populated
//	confirm_plan    everything is known and nothing has been built yet.
//	                Assistant carries the recap, the honest verdict on whether
//	                the goal fits the time, and the go-ahead question; Options
//	                are the ways forward. NO plan exists until the user
//	                approves, so a client must render this stage — treating it
//	                as an ordinary message leaves the conversation waiting for
//	                a reply it never sends.
//	plan_ready      PlanID is set and Done is true; fetch GET /api/plan/{id}
//
// plan_ready is NOT the end of the conversation. The same endpoint keeps
// answering questions about the plan ("why Writing on Wednesday?", "what should
// I do today?") and keeps accepting changes to it ("I can't do Tuesdays any
// more"). A turn that changed something says so in PlanChanged /
// ScheduleChanged / ChangeType, and the plan at PlanID is the updated one.
//
// Assistant is the full prose for the chat bubble and may carry a lead-in or a
// skill primer. Question is the bare question for stages that ask one, so a
// client can render it in a dedicated control without splitting strings.
type Turn struct {
	SessionID string    `json:"sessionId"`
	Stage     string    `json:"stage"`
	Assistant string    `json:"assistant"`
	Question  string    `json:"question,omitempty"`
	Options   []string  `json:"options"`
	Progress  *Progress `json:"progress,omitempty"`
	PlanID    string    `json:"planId,omitempty"`
	Done      bool      `json:"done"`

	// Change metadata, additive and omitted when nothing happened, so an
	// existing client is unaffected and a new one can refresh exactly what
	// moved instead of refetching everything on every message.
	PlanChanged     bool   `json:"planChanged,omitempty"`
	ScheduleChanged bool   `json:"scheduleChanged,omitempty"`
	ChangeType      string `json:"changeType,omitempty"`
}

// Progress describes how far the intake interview has got. The interview is
// adaptive and normally stops early, so Max is a ceiling the backend guarantees
// it will not exceed, NOT a promise of how many questions will be asked.
// Adaptive is always true and exists so a client cannot mistake Max for a total.
type Progress struct {
	Answered int  `json:"answered"`
	Max      int  `json:"max"`
	Adaptive bool `json:"adaptive"`
}

// intakeTurn builds an intake Turn with the contract fields filled in.
func (p *Pipeline) intakeTurn(sess *IntakeSession, assistant, question string, options []string) Turn {
	return Turn{
		Stage:     "intake",
		Assistant: assistant,
		Question:  question,
		Options:   options,
		Progress: &Progress{
			Answered: clamp(sess.AskedCount, 0, maxIntakeQuestions),
			Max:      maxIntakeQuestions,
			Adaptive: true,
		},
	}
}

type feasibilityOption struct {
	Label        string   `json:"label"`
	Target       string   `json:"target"`
	Deadline     string   `json:"deadline"`
	HoursPerWeek int      `json:"hoursPerWeek"`
	Days         []string `json:"days"`
}

type feasibilityDecision struct {
	Resolved bool `json:"resolved"`
	// Approved is the actual go-ahead. Resolved only means the reply was
	// understood: "make it 10 hours" is resolved but not approved, and leads
	// to a fresh recap rather than straight to a plan.
	Approved bool   `json:"approved"`
	Target   string `json:"target"`
	Deadline string `json:"deadline"`
	// Days was missing here, and a user who changed their study days at this
	// gate had the change acknowledged in the recap and then thrown away: the
	// scheduler went on using the days from the interview, so the calendar
	// disagreed with the summary they had just approved.
	HoursPerWeek int      `json:"hoursPerWeek"`
	Days         []string `json:"days"`
	KeepOriginal bool     `json:"keepOriginal"`
}

// Feasibility verdicts. "Reachable: yes/no" was the wrong shape: required study
// hours are ESTIMATES with wide error bars, so a boolean forced the assistant
// to choose between endorsing a timeline and telling someone they cannot reach
// IELTS 7 — a claim the arithmetic never supported. These four say what is
// actually known.
const (
	feasibleStatus     = "feasible"     // capacity comfortably covers the estimate
	tightStatus        = "tight"        // capacity is close to the low estimate
	insufficientStatus = "insufficient" // scheduled capacity falls short under current assumptions
	unknownStatus      = "unknown"      // availability or target is not established
)

func normalizeFeasibilityStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case feasibleStatus, tightStatus, insufficientStatus, unknownStatus:
		return strings.ToLower(strings.TrimSpace(s))
	default:
		return unknownStatus
	}
}

type feasibilityResult struct {
	// RequiredHoursLow/High are the estimate as a RANGE, because that is what
	// it is. RequiredHours is kept for compatibility and is read as the midpoint
	// when the range is absent.
	RequiredHours     int `json:"requiredHours"`
	RequiredHoursLow  int `json:"requiredHoursLow"`
	RequiredHoursHigh int `json:"requiredHoursHigh"`
	// AvailableHours is overwritten by the backend with its own computed
	// capacity: the model may describe it but never decides it.
	AvailableHours int `json:"availableHours"`
	// Status is one of feasible|tight|insufficient|unknown.
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Verdict string `json:"verdict"`
	// Projection is written by the BACKEND, never the model: when no deadline
	// exists it states the projected finish computed from the learner's own
	// availability, explicitly labelled as a projection.
	Projection string              `json:"-"`
	Question   string              `json:"question"`
	Options    []feasibilityOption `json:"options"`
	Decision   feasibilityDecision `json:"decision"`

	// The confirmation step answers questions too — "why so many weeks?" is a
	// fair thing to ask of a recap, and must not read as a refusal.
	LatestWasQuestion bool   `json:"latestWasQuestion"`
	ReplyToUser       string `json:"replyToUser"`
	NoteForPlan       string `json:"noteForPlan"`
}

type Pipeline struct {
	cfg   Config
	store *Store
	gw    *Gateway
	sched *Scheduler
	// res looks up real products and courses. It starts with no providers
	// registered, which is the honest default: see resource_providers.go. Every
	// call through it is enrichment and every failure is survivable.
	res *ResourceService
}

func newPipeline(cfg Config, store *Store, gw *Gateway, sched *Scheduler) *Pipeline {
	return &Pipeline{cfg: cfg, store: store, gw: gw, sched: sched, res: newResourceService()}
}

// maxIntakeQuestions bounds the interview so it always terminates. It is a
// backstop only: intakeCategories is what actually ends the interview, and the
// ceiling sits above the number of categories so it never cuts one off.
const maxIntakeQuestions = 8

// ---- what the interview must establish ----
//
// These are the facts a plan cannot be built without. Every one of them feeds
// something concrete: timeBudget and deadline size and date the whole schedule,
// budget bounds the setup items, currentLevel and target define the gap. The
// list is ordered by how much each one reshapes the plan.

type intakeCategory struct {
	key string
	// filled reports whether this category already has an answer, whether the
	// user volunteered it or a previous goal put it on their profile.
	filled func(*IntakeSession) bool
	// maxAsks is how many times this category may be put to the user. One for
	// almost everything: a blank answer is still an answer. timeBudget gets two
	// because it is the one category that can be HALF answered — "Monday and
	// Saturday" with no hours, or "5 hours a week" with no days — and a plan
	// cannot be scheduled, or honestly assessed, on half of it.
	maxAsks int
}

var intakeCategories = []intakeCategory{
	{"pivotalChoice", func(s *IntakeSession) bool {
		// Only a real category when the understand stage found a fork to resolve.
		return s.PivotalChoice == "" || s.Answers.PivotalChoice != ""
	}, 1},
	{"currentLevel", func(s *IntakeSession) bool { return s.Answers.CurrentLevel != "" }, 1},
	{"target", func(s *IntakeSession) bool { return s.Answers.Target != "" }, 1},
	{"timeBudget", func(s *IntakeSession) bool {
		// Days and time are one question: "5 hours, weekends" answers both, and
		// splitting them wastes a turn on half an availability. But BOTH halves
		// are required — this is the availability the whole feasibility
		// calculation rests on, and guessing either one is what let a plan be
		// sized against time the learner never had.
		return currentAvailability(s).Complete()
	}, 2},
	{"deadline", func(s *IntakeSession) bool { return s.Answers.Deadline != "" }, 1},
	{"budget", func(s *IntakeSession) bool { return s.Answers.Budget != "" }, 1},
}

func isIntakeCategory(key string) bool {
	for _, c := range intakeCategories {
		if c.key == key {
			return true
		}
	}
	return false
}

// askedCategoryKey namespaces the marker inside AnswerBag, which the mock brain
// also writes to under its own key names.
func askedCategoryKey(key string) string { return "asked_" + key }

// askCount reports how many times a category has been put to the user. The
// marker used to be a bare "yes"; a count is stored now so timeBudget can have
// a second attempt without any category becoming unbounded.
func askCount(sess *IntakeSession, key string) int {
	switch v := sess.AnswerBag[askedCategoryKey(key)]; v {
	case "":
		return 0
	case "yes":
		return 1 // the pre-existing marker shape
	default:
		return maxInt(1, atoi(v))
	}
}

func categoryExhausted(sess *IntakeSession, c intakeCategory) bool {
	limit := c.maxAsks
	if limit <= 0 {
		limit = 1
	}
	return askCount(sess, c.key) >= limit
}

// nextIntakeCategory returns what the next question must cover, or "" when the
// interview is over. A category already put to the user is not raised again
// beyond its allowance: "no deadline in mind" is a complete answer, and
// re-asking would spend the whole interview on one field.
func nextIntakeCategory(sess *IntakeSession) string {
	for _, c := range intakeCategories {
		if c.filled(sess) || categoryExhausted(sess, c) {
			continue
		}
		return c.key
	}
	return ""
}

// intakeCategoryMeaning travels with the payload so the model writes a question
// about the right thing without the prompt having to re-describe each category.
var intakeCategoryMeaning = map[string]string{
	"pivotalChoice": "which side of the fork named in pivotalChoice they want",
	"currentLevel":  "where they are with this skill today, concretely",
	"target":        "the specific outcome they want to reach",
	"timeBudget":    "which days of the week they can study AND how much time — one question covering both",
	"deadline":      "the date they want to be done by",
	"budget":        "how much money they can put into this",
}

// timeBudgetMeaning narrows the availability question to whichever half is
// still missing. Asking "how many hours and which days?" of someone who has
// already said "Monday and Saturday" is the kind of redundancy that makes an
// assistant feel like a form.
func timeBudgetMeaning(sess *IntakeSession) string {
	switch missingAvailability(sess.Answers.Availability) {
	case "days":
		return "which days of the week they can study — they have already told you how much time, so do NOT ask about hours again"
	case "time":
		return "how much time they can study on those days (per day or per week) — they have already told you which days, so do NOT ask which days again"
	default:
		return intakeCategoryMeaning["timeBudget"]
	}
}

// remainingCategories lists what is still outstanding after askAbout, so a
// reply that answers more than it was asked can move straight on.
func remainingCategories(sess *IntakeSession, askAbout string) []string {
	out := []string{}
	seen := false
	for _, c := range intakeCategories {
		if c.key == askAbout {
			seen = true
			continue
		}
		if !seen || c.filled(sess) || categoryExhausted(sess, c) {
			continue
		}
		out = append(out, c.key)
	}
	return out
}

func markCategoryAsked(sess *IntakeSession, key string) {
	if key == "" {
		return
	}
	if sess.AnswerBag == nil {
		sess.AnswerBag = map[string]string{}
	}
	sess.AnswerBag[askedCategoryKey(key)] = itoa(askCount(sess, key) + 1)
}

// ---- prompt context ----
//
// Every stage used to see one bare string, which left the model unable to do
// things the prompts ask of it: resolve "in three months" without today's
// date, avoid re-asking a question it could not see it had asked, or size a
// week of work against a budget it was never told. These builders assemble the
// JSON payload each stage documents, and because the gateway keys its cache on
// that payload, richer context also means a correctly narrower cache.

// promptMessage is one turn as a stage sends it to the provider.
//
// The transcript is no longer pasted into the stage payload. It is replayed as
// real user/assistant messages (see conversation.go), bounded by both a turn
// count and a character budget, and it never contains the message currently
// being handled — that travels alone in <current_user_message>. The old
// behaviour sent both, so the newest user message arrived twice.
type promptMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// sessionUser returns the session's stored profile (nil when absent) and the
// location its timezone names, so "today" is today where the user actually is.
func (p *Pipeline) sessionUser(sess *IntakeSession) (*User, *time.Location) {
	u, ok := p.store.GetUser(sess.UserID)
	if !ok {
		return nil, loadLocation("")
	}
	return u, loadLocation(u.Timezone)
}

// planAvailability resolves the weekly budget the plan is built against, and —
// critically — says whether it was actually told or merely assumed.
//
// The scheduler must always be handed something placeable, so a fallback still
// exists. What changed is that the fallback is no longer invisible: assumed ==
// true means nobody chose these numbers, and the confirmation gate refuses to
// present feasibility arithmetic computed from them. Treating the default six
// hours as a fact is half of how a deadline turned into a capacity estimate.
func planAvailability(sess *IntakeSession) (av StudyAvailability, assumed bool) {
	av = currentAvailability(sess)
	if av.Complete() {
		return av, false
	}
	if !av.HasDays() {
		av.Days = []string{"Mon", "Wed", "Fri"}
	}
	if !av.HasTime() {
		av.WeeklyMinutes = defaultHoursWeek * 60
	}
	av.normalize()
	return av, true
}

func (p *Pipeline) buildUnderstandContext(sess *IntakeSession, msg string) string {
	_, loc := p.sessionUser(sess)
	b, _ := json.Marshal(map[string]any{
		"today":      dateStr(todayIn(loc)),
		"knownSkill": sess.Skill,
	})
	return string(b)
}

func (p *Pipeline) buildIntakeContext(sess *IntakeSession, askAbout string) string {
	u, loc := p.sessionUser(sess)
	profile := map[string]any{"hoursPerWeek": 0, "days": []string{}, "timezone": ""}
	if u != nil {
		// Availability carried over from an earlier goal. The prompt treats a
		// non-empty value as already known, which is what UpdateUserAvailability
		// was saving it for — until now nothing ever handed it to the model.
		profile["hoursPerWeek"] = u.HoursPerWeek
		if len(u.Days) > 0 {
			profile["days"] = u.Days
		}
		profile["timezone"] = u.Timezone
	}
	meaning := map[string]string{}
	for k, v := range intakeCategoryMeaning {
		meaning[k] = v
	}
	meaning["timeBudget"] = timeBudgetMeaning(sess)

	av := currentAvailability(sess)
	b, _ := json.Marshal(map[string]any{
		"today":         dateStr(todayIn(loc)),
		"skill":         sess.Skill,
		"path":          sess.Path,
		"pivotalChoice": sess.PivotalChoice,
		"knownAnswers":  sess.Answers,
		"profile":       profile,
		// Availability, spelled out, so the model can see which half it already
		// has and never asks for it twice.
		"availability": map[string]any{
			"days":          av.Days,
			"weeklyMinutes": av.WeeklyMinutes,
			"perDay":        av.PerDay,
			"missing":       missingAvailability(av),
		},
		// The backend's choice of subject for this turn, and what is still
		// outstanding after it, so a reply that answers ahead can skip forward.
		"askAbout":        askAbout,
		"askCategories":   append([]string{askAbout}, remainingCategories(sess, askAbout)...),
		"categoryMeaning": meaning,
		// The interview is cut off at maxIntakeQuestions whatever the model
		// wants, so it has to know how much room is left to spend.
		"questionsAsked":     sess.AskedCount,
		"questionsRemaining": maxInt(0, maxIntakeQuestions-sess.AskedCount),
	})
	return string(b)
}

// planHorizon resolves the schedule arithmetic both the feasibility gate and
// the plan stage reason about, from one place so the two can never disagree
// about how much time the user actually has.
//
// THE CAPACITY RULE. studyHours is computed by walking the learner's own
// availability across the calendar (availableStudyMinutes), NOT from the span
// to the deadline. A deadline ten weeks out at three hours a week is thirty
// study hours; it is not 10 x 7 x 24, and it is not ten weeks of a default
// nobody chose. capacityKnown is false whenever the availability was assumed,
// and every consumer is required to treat that as "unknown", not as zero and
// not as a number to reason from.
type horizon struct {
	Start        time.Time
	Availability StudyAvailability
	Assumed      bool
	// HasDeadline is whether the LEARNER gave one. A projected finish date is
	// not a deadline and never sets this.
	HasDeadline        bool
	Deadline           time.Time
	WeeksUntilDeadline int
	StudyMinutes       int
	CapacityKnown      bool
	PlanWeeks          int
}

func planHorizon(sess *IntakeSession, loc *time.Location) horizon {
	h := horizon{}
	h.Availability, h.Assumed = planAvailability(sess)
	h.Start = todayIn(loc).AddDate(0, 0, 1) // same first day materializePlan uses

	if d, ok := parseDateIn(sess.Answers.Deadline, loc); ok {
		h.HasDeadline = true
		h.Deadline = d
		if n := daysBetween(h.Start, d); n > 0 {
			h.WeeksUntilDeadline = (n + 6) / 7 // whole weeks, rounded up
		}
		if !h.Assumed {
			h.StudyMinutes, h.CapacityKnown = availableStudyMinutes(h.Availability, h.Start, d)
		}
	}
	// NO fallback horizon. "Study time available before your deadline" is
	// meaningless without a deadline, and computing it against maxPlanWeeks
	// invented one: fifty-two weeks at two hours produced the "103 available
	// hours" a learner was shown who had never given a date at all. Without a
	// deadline, capacity stays unknown and a PROJECTED FINISH is offered
	// instead — see projectionLine.

	h.PlanWeeks = maxPlanWeeks
	if h.WeeksUntilDeadline > 0 {
		h.PlanWeeks = minInt(h.WeeksUntilDeadline, maxPlanWeeks)
	}
	return h
}

// projectedWeeks is how long a workload takes at the learner's own rate.
// Deterministic: the model supplies the workload estimate, the backend does the
// division. Zero when either side is unknown.
func projectedWeeks(requiredHours int, av StudyAvailability) int {
	if requiredHours <= 0 || !av.Complete() {
		return 0
	}
	weeks := (requiredHours*60 + av.WeeklyMinutes - 1) / av.WeeklyMinutes
	return maxInt(1, weeks)
}

// projectionLine is the backend's own sentence about when a plan would finish
// when no deadline exists.
//
// It is labelled as a projection every time. A date start.ai worked out from an
// estimate is not a commitment the learner made, and presenting one as
// "Deadline: <date>" would put words in their mouth.
func projectionLine(lang string, h horizon, lowHours, highHours int) string {
	if h.HasDeadline || h.Assumed || !h.Availability.Complete() {
		return ""
	}
	lo, hi := projectedWeeks(lowHours, h.Availability), projectedWeeks(highHours, h.Availability)
	if lo == 0 && hi == 0 {
		return ""
	}
	if lo == 0 {
		lo = hi
	}
	if hi == 0 {
		hi = lo
	}
	if hi < lo {
		lo, hi = hi, lo
	}
	hours := itoa(h.Availability.HoursPerWeek())
	from := dateStr(h.Start.AddDate(0, 0, lo*7))
	to := dateStr(h.Start.AddDate(0, 0, hi*7))
	span := itoa(lo) + "–" + itoa(hi)
	if lo == hi {
		span = itoa(lo)
	}
	return tr(lang,
		"You haven't set a deadline, so there's nothing to fall short of. At about "+hours+"h a week that works out at roughly "+span+" weeks — a projected finish around "+from+" to "+to+". That's an estimate from your availability, not a date you've committed to.",
		"Вы не указали срок, поэтому и отставать не от чего. При примерно "+hours+" ч в неделю это около "+span+" недель — ориентировочное завершение между "+from+" и "+to+". Это оценка по вашей доступности, а не дата, которую вы назначили.",
		"Siz muddat belgilamagansiz, shuning uchun kechikadigan narsa ham yo'q. Haftasiga taxminan "+hours+" soat bilan bu taxminan "+span+" hafta — taxminiy tugash "+from+" va "+to+" oralig'ida. Bu sizning imkoniyatingizdan chiqarilgan taxmin, o'zingiz belgilagan sana emas.")
}

// studyHours renders the capacity as whole hours, or -1 when it is unknown.
// -1 rather than 0 because zero is a real answer ("your deadline is tomorrow")
// and must not be confused with "we have not asked yet".
func (h horizon) studyHours() int {
	if !h.CapacityKnown {
		return -1
	}
	return h.StudyMinutes / 60
}

func (p *Pipeline) buildFeasibilityContext(sess *IntakeSession, loc *time.Location) string {
	h := planHorizon(sess, loc)
	av := h.Availability
	b, _ := json.Marshal(map[string]any{
		"today":        dateStr(todayIn(loc)),
		"skill":        sess.Skill,
		"path":         sess.Path,
		"currentLevel": sess.Answers.CurrentLevel,
		"target":       sess.Answers.Target,
		"deadline":     sess.Answers.Deadline,

		// Availability, as stated by the learner.
		"days":          av.Days,
		"weeklyMinutes": av.WeeklyMinutes,
		"hoursPerWeek":  av.HoursPerWeek(),
		"perDay":        av.PerDay,

		"weeksUntilDeadline": h.WeeksUntilDeadline,
		"maxWeeks":           maxPlanWeeks,

		// hasDeadline is whether the LEARNER gave one. When it is false there
		// is no date to be early or late for, so nothing may be described as
		// insufficient "before the deadline" and no option may propose moving
		// one. A projected finish is offered instead, and the backend writes it.
		"hasDeadline": h.HasDeadline,

		// The ONLY capacity figure. It is the sum of the learner's own
		// schedulable minutes between the start date and the deadline.
		// capacityKnown=false means availability or the deadline is missing and
		// no feasibility claim of any kind may be made.
		"availableStudyHours": h.studyHours(),
		"capacityKnown":       h.CapacityKnown,

		// The full answer set, so the recap can be checked against everything
		// the user actually said rather than the handful of scheduling fields.
		"answers":   sess.Answers,
		"planNotes": sess.PlanNotes,
	})
	return string(b)
}

func (p *Pipeline) buildPlanContext(sess *IntakeSession, loc *time.Location) string {
	h := planHorizon(sess, loc)
	av := h.Availability

	b, _ := json.Marshal(map[string]any{
		"today":              dateStr(todayIn(loc)),
		"startDate":          dateStr(h.Start),
		"skill":              sess.Skill,
		"path":               sess.Path,
		"answers":            sess.Answers,
		"weeklyMinuteBudget": av.WeeklyMinutes,
		"hoursPerWeek":       av.HoursPerWeek(),
		"days":               av.Days,
		"daysAvailable":      len(av.Days),
		"perDay":             av.PerDay,
		"deadline":           sess.Answers.Deadline,
		"weeksUntilDeadline": h.WeeksUntilDeadline,
		// The study hours the learner's OWN availability yields before the
		// deadline. Handing the model the finished number is what lets it check
		// a target against reality instead of calling every timeline
		// "achievable". -1 means availability is unknown, not zero.
		"availableStudyHours": h.studyHours(),
		"capacityKnown":       h.CapacityKnown,
		"maxWeeks":            maxPlanWeeks,
		// The verdict the user was shown and approved at the confirmation gate.
		// The plan must be built for what was agreed, and its own feasibility
		// text must not contradict what they already accepted.
		"agreedFeasibility": sess.FeasibilityNote,
		// Things the user asked for along the way. They approved a plan that
		// was supposed to honour these, so the plan has to actually honour them.
		"userRequests": sess.PlanNotes,
	})
	return string(b)
}

// HandleChat advances the intake state machine by one user message.
//
// The caller must hold the per-session lock: a conversation is inherently
// sequential, and without that serialization concurrent turns lose AskedCount
// increments and can build two plans for one session.
func (p *Pipeline) HandleChat(ctx context.Context, sess *IntakeSession, userMsg string) (Turn, error) {
	if sess.Stage == "" {
		sess.Stage = "scope_check"
	}
	// Record what the user said BEFORE doing any work, exactly once. Every
	// stage reads history through conversationWindow, which excludes this
	// message and passes it separately, so an internal retry or a fallback to
	// another model cannot append it a second time.
	entryStage := sess.Stage
	appendUserMessage(sess, userMsg, entryStage)

	var turn Turn
	var err error
	switch entryStage {
	case "scope_check", "out_of_scope":
		turn, err = p.doUnderstand(ctx, sess, userMsg)
	case "disambiguation":
		turn, err = p.doDisambiguation(ctx, sess, userMsg)
	case "intake":
		turn, err = p.doIntake(ctx, sess, userMsg)
	case "confirm_plan":
		turn, err = p.doConfirm(ctx, sess, userMsg)
	case "plan_ready":
		// A finished plan is the START of the relationship, not the end of it.
		turn, err = p.doAssist(ctx, sess, userMsg)
	default:
		turn, err = p.doUnderstand(ctx, sess, userMsg)
	}
	if err != nil {
		// A failed turn records NO assistant message: inventing a successful
		// reply that was never produced would poison every later turn's memory.
		// The user message stays — they really did say it — and the session is
		// persisted so a retry sees the same history rather than a fresh one.
		p.store.SaveSession(sess)
		return Turn{}, err
	}

	turn.SessionID = sess.ID
	appendAssistantMessage(sess, turn.Assistant, turn.Stage)
	p.store.SaveSession(sess)
	return turn, nil
}

func (p *Pipeline) doUnderstand(ctx context.Context, sess *IntakeSession, msg string) (Turn, error) {
	var u understandResult
	if p.gw.Enabled() {
		payload := p.buildUnderstandContext(sess, msg)
		history := conversationWindow(sess)
		turns := stageTurns(prompts["understand"].System, sess.Lang, history, payload, msg,
			"Classify the current user message: is it a learning goal, does it need narrowing, and what is the skill?")
		raw, err := p.gw.ChatTurns(ctx, sess.UserID, p.cfg.ModelFast, turns, true,
			cacheKeyForTurns("understand", sess.Lang, payload+"\x00"+msg, history))
		if err != nil {
			return Turn{}, err
		}
		// A malformed live response is a live failure. Quietly substituting the
		// mock brain here made a broken model indistinguishable from a working
		// one, in production as well as in demos.
		if e := json.Unmarshal([]byte(extractJSONObject(raw)), &u); e != nil {
			return Turn{}, fmt.Errorf("%w: understand stage returned unparsable JSON: %v", errAIUnavailable, e)
		}
	} else {
		u = mockUnderstand(msg, sess.Lang)
	}

	if !u.InScope {
		sess.Stage = "out_of_scope"
		fallback := tr(sess.Lang,
			"I can only help you learn a skill — tell me what you'd like to learn.",
			"Я помогаю только с обучением навыкам — скажите, что вы хотите освоить.",
			"Men faqat ko'nikma o'rganishda yordam beraman — nimani o'rganmoqchi ekaningizni ayting.")
		return Turn{Stage: sess.Stage, Assistant: firstNonEmpty(u.Decline, fallback)}, nil
	}

	sess.Skill = u.Skill
	sess.PivotalChoice = u.PivotalChoice
	sess.Overview = u.Overview

	// Record the goal once per session; restating an in-scope goal updates it
	// rather than accumulating orphaned Goal rows.
	g := &Goal{
		ID: firstNonEmpty(sess.GoalID, newID("goal")), UserID: sess.UserID,
		RawInput: msg, Skill: u.Skill, Path: sess.Path, CreatedAt: time.Now(),
	}
	sess.GoalID = g.ID
	p.store.SaveGoal(g)

	if u.NeedsDisambiguation {
		sess.NeedsDisambiguation = true
		sess.Stage = "disambiguation"
		return Turn{Stage: sess.Stage, Assistant: u.DisambiguationQuestion, Question: u.DisambiguationQuestion, Options: u.Options}, nil
	}

	sess.Stage = "intake"
	// The goal message itself is handed to the interview: "I am A1 and want C1
	// by next summer" answers three categories before a single question is
	// asked, and passing "" here meant none of them were ever extracted.
	first, err := p.firstIntakeTurn(ctx, sess, msg)
	if err != nil {
		return Turn{}, err
	}
	intro := strings.TrimSpace(u.Overview)
	if intro != "" {
		first.Assistant = intro + "\n\n" + first.Assistant
	}
	return first, nil
}

func (p *Pipeline) doDisambiguation(ctx context.Context, sess *IntakeSession, msg string) (Turn, error) {
	sess.Path = sanitizeSkillLabel(msg)
	sess.NeedsDisambiguation = false
	sess.Stage = "intake"
	return p.firstIntakeTurn(ctx, sess, msg)
}

func (p *Pipeline) firstIntakeTurn(ctx context.Context, sess *IntakeSession, latest string) (Turn, error) {
	r, err := p.nextIntake(ctx, sess, latest, true)
	if err != nil {
		return Turn{}, err
	}
	// A first message can carry everything ("beginner, 5 songs by December, 5
	// hours at weekends, $100, video lessons"), and the interview is then over
	// before it starts. Only doIntake used to honour Done, so that user was
	// shown an empty question bubble and left stuck in the intake stage.
	if r.Done {
		return p.gateBeforePlan(ctx, sess)
	}
	lead := tr(sess.Lang,
		"Let's tailor your plan for "+sess.Skill+". ",
		"Давайте настроим ваш план для «"+sess.Skill+"». ",
		"Keling, «"+sess.Skill+"» uchun rejangizni moslaymiz. ")
	return p.intakeTurn(sess, lead+r.NextQuestion, r.NextQuestion, r.Options), nil
}

func (p *Pipeline) doIntake(ctx context.Context, sess *IntakeSession, msg string) (Turn, error) {
	r, err := p.nextIntake(ctx, sess, msg, false)
	if err != nil {
		return Turn{}, err
	}

	// Asking a question is not spending one of the interview's turns. Counting
	// it would let a curious user exhaust the budget and be handed a plan built
	// on half an interview.
	if !r.LatestWasQuestion {
		sess.AskedCount++
	}

	if r.Done || sess.AskedCount >= maxIntakeQuestions {
		return p.gateBeforePlan(ctx, sess)
	}
	return p.intakeTurn(sess, withReply(r.ReplyToUser, r.NextQuestion), r.NextQuestion, r.Options), nil
}

// ---- the confirmation gate ----
//
// Between the interview and the plan sits the step nobody was taking: showing
// the user what was understood and asking whether to go ahead. Two things used
// to go wrong here. A plan appeared the instant the last answer landed, built
// from a recap the user never saw and could not correct. And the question of
// whether the goal was even reachable in the time available was answered inside
// the finished plan, as a sentence read after the fact — so someone asking for
// C1 Russian in eleven months was handed a schedule that quietly could not
// deliver it.
//
// Now the recap and the arithmetic are put to the user together and the
// pipeline stops. Nothing is built until they say go ahead — and when the
// numbers do not support the goal, the alternatives come with it.

// gateBeforePlan presents the recap and waits. It never plans on its own.
//
// It also refuses to present arithmetic it cannot support. Availability is the
// input the entire feasibility calculation rests on, so if it is still missing
// the gate sends the interview back for it rather than quietly substituting a
// default and showing the user a capacity nobody ever stated. The interview
// ceiling still applies: once it is spent the plan is built on an explicitly
// assumed availability and the verdict says "unknown" rather than pretending.
func (p *Pipeline) gateBeforePlan(ctx context.Context, sess *IntakeSession) (Turn, error) {
	if sess.FeasibilityAgreed {
		return p.finishIntakeAndPlan(ctx, sess)
	}
	if !currentAvailability(sess).Complete() && sess.AskedCount < maxIntakeQuestions {
		if turn, asked, err := p.askForAvailability(ctx, sess); asked || err != nil {
			return turn, err
		}
	}
	r, err := p.confirmBeforePlan(ctx, sess, "")
	if err != nil {
		return Turn{}, err
	}
	sess.FeasibilityNote = r.Verdict
	sess.FeasibilityStatus = normalizeFeasibilityStatus(r.Status)
	sess.Stage = "confirm_plan"
	return confirmTurn(sess, r), nil
}

// askForAvailability puts the missing half of the availability question and
// keeps the conversation in the intake stage. It returns asked=false when the
// category has already had its allowance, so this can never loop.
func (p *Pipeline) askForAvailability(ctx context.Context, sess *IntakeSession) (Turn, bool, error) {
	for _, c := range intakeCategories {
		if c.key != "timeBudget" {
			continue
		}
		if categoryExhausted(sess, c) {
			return Turn{}, false, nil
		}
	}
	sess.Stage = "intake"
	r, err := p.nextIntake(ctx, sess, "", false)
	if err != nil {
		return Turn{}, false, err
	}
	if r.Done || strings.TrimSpace(r.NextQuestion) == "" {
		// The stage decided nothing is outstanding after all.
		return Turn{}, false, nil
	}
	sess.AskedCount++
	return p.intakeTurn(sess, withReply(r.ReplyToUser, r.NextQuestion), r.NextQuestion, r.Options), true, nil
}

// doConfirm handles the user's answer to the recap: go ahead, change something,
// ask a question, or insist on a goal the numbers do not support.
func (p *Pipeline) doConfirm(ctx context.Context, sess *IntakeSession, msg string) (Turn, error) {
	r, err := p.confirmBeforePlan(ctx, sess, msg)
	if err != nil {
		return Turn{}, err
	}
	if note := strings.TrimSpace(r.NoteForPlan); note != "" {
		sess.PlanNotes = appendNote(sess.PlanNotes, note)
	}
	applyConfirmOutcome(sess, r)

	// An adjustment they asked for is applied whether or not they also approved,
	// so "make it 10 hours a week, go ahead" does both in one message.
	changed := false
	if r.Decision.Resolved && !r.Decision.KeepOriginal {
		before := answersFingerprint(sess)
		applyFeasibilityDecision(sess, r.Decision, msg)
		changed = before != answersFingerprint(sess)
	}

	// Only an explicit go-ahead builds anything. A question, a change or an
	// unclear reply all come back here for another look.
	if !r.Decision.Resolved || !r.Decision.Approved {
		// The recap they were just shown was computed from the values as they
		// stood BEFORE this message, so a change leaves its arithmetic one step
		// behind — "8 hours a week" above a total that still assumes 5. Redo it
		// against what they actually have now, since this is the version they
		// are being asked to approve.
		if changed {
			fresh, err := p.confirmBeforePlan(ctx, sess, "")
			if err != nil {
				return Turn{}, err
			}
			fresh.ReplyToUser = firstNonEmpty(r.ReplyToUser, fresh.ReplyToUser)
			applyConfirmOutcome(sess, fresh)
			return confirmTurn(sess, fresh), nil
		}
		return confirmTurn(sess, r), nil
	}
	sess.FeasibilityAgreed = true
	sess.Stage = "intake"
	return p.finishIntakeAndPlan(ctx, sess)
}

func confirmTurn(sess *IntakeSession, r feasibilityResult) Turn {
	labels := make([]string, 0, len(r.Options)+2)
	for _, o := range r.Options {
		if s := strings.TrimSpace(o.Label); s != "" {
			labels = append(labels, s)
		}
	}
	// A recap the user is expected to approve needs something to approve with.
	if len(labels) == 0 {
		labels = append(labels,
			tr(sess.Lang, "Yes, build my plan", "Да, составьте план", "Ha, rejamni tuzing"),
			tr(sess.Lang, "Change something", "Изменить кое-что", "Biror narsani o'zgartirish"))
	}
	question := strings.TrimSpace(r.Question)
	if question == "" {
		question = tr(sess.Lang,
			"Shall I build your plan from this?",
			"Составить план на этой основе?",
			"Shu asosda rejangizni tuzaymi?")
	}
	parts := []string{}
	for _, s := range []string{r.ReplyToUser, r.Summary, r.Verdict, r.Projection, question} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	return Turn{
		Stage:     "confirm_plan",
		Assistant: strings.Join(parts, "\n\n"),
		Question:  question,
		Options:   labels,
	}
}

// applyFeasibilityDecision writes the agreed goal back over the intake answers,
// validating exactly as applyAnswers does so a bad date or a silly number of
// hours cannot enter through this door instead.
// applyFeasibilityDecision writes an agreed change back over the intake answers.
//
// userMsg is the learner's own message for this turn, and it gates the
// availability half: the decision may only move the study week when the learner
// actually said something about their study week. The confirm prompt asks for
// the COMPLETE resulting set on every decision, including approvals, so a model
// that re-reads an older recap out of the transcript will cheerfully "confirm"
// the numbers the learner already replaced. That is how a recap showing 14h/week
// across seven days was approved and then built as two hours on Mondays.
//
// Target and deadline are applied from the decision as before: an option like
// "Lower the target to basic syntax" carries no numbers for a parser to find,
// and getting those wrong is visible in the next recap rather than silently
// baked into a year of calendar.
func applyFeasibilityDecision(sess *IntakeSession, d feasibilityDecision, userMsg string) {
	a := &sess.Answers
	if v := strings.TrimSpace(d.Target); v != "" {
		a.Target = v
	}
	if v := strings.TrimSpace(d.Deadline); validDate(v) {
		a.Deadline = v
	}

	// Did the learner state an availability in this message? Option labels go
	// through this same path rather than a parser of their own, so selecting
	// "Increase study time to 35 hours per week (5 h/day)" mutates state
	// exactly as typing it would.
	st := parseAvailabilityStatement(userMsg)
	if st.empty() {
		return
	}

	// Their words first, resolved against the week in force; the decision may
	// only fill a half those words left unread. Everything goes through the
	// same normalization as an interview answer, so an unparseable day name
	// changes nothing rather than emptying the week, and a silly number of
	// hours is clamped.
	// allowBareNumber is false here on purpose: the gate never asks "how many
	// hours?", so a lone digit in a message at this point belongs to something
	// else — a target, a band score, an option number.
	next := resolveAvailability(currentAvailability(sess), st, false)
	var fill StudyAvailability
	if !next.HasDays() && len(d.Days) > 0 {
		fill.Days = detectDays(strings.Join(d.Days, ","))
	}
	if !next.HasTime() && d.HoursPerWeek > 0 {
		fill.WeeklyMinutes = clamp(d.HoursPerWeek, 1, 40) * 60
	}
	fill.normalize()
	if fill.HasDays() || fill.HasTime() {
		next = mergeAvailability(next, fill)
	}
	syncAnswersAvailability(sess, next)
}

func (p *Pipeline) confirmBeforePlan(ctx context.Context, sess *IntakeSession, latest string) (feasibilityResult, error) {
	_, loc := p.sessionUser(sess)
	h := planHorizon(sess, loc)
	if !p.gw.Enabled() {
		return mockConfirm(sess, h, latest), nil
	}
	payload := p.buildFeasibilityContext(sess, loc)
	history := conversationWindow(sess)
	task := "Recap what you understood, state the capacity arithmetic honestly, and ask whether to build the plan. Read the current user message as their answer if there is one."
	turns := stageTurns(prompts["confirm"].System, sess.Lang, history, payload, latest, task)
	raw, err := p.gw.ChatTurns(ctx, sess.UserID, p.cfg.ModelSmart, turns, true,
		cacheKeyForTurns("confirm", sess.Lang, payload+"\x00"+latest, history))
	if err != nil {
		return feasibilityResult{}, err
	}
	var r feasibilityResult
	if e := json.Unmarshal([]byte(extractJSONObject(raw)), &r); e != nil {
		return feasibilityResult{}, fmt.Errorf("%w: confirm stage returned unparsable JSON: %v", errAIUnavailable, e)
	}
	// The capacity figure is the backend's, always. The model can describe it;
	// it does not get to restate it, round it, or derive one of its own from
	// the deadline.
	r.AvailableHours = h.studyHours()
	r.Status = normalizeFeasibilityStatus(r.Status)
	if !h.CapacityKnown {
		r.Status = unknownStatus
	}
	// Without a deadline there is nothing to fall short of, so the backend
	// replaces any shortfall verdict with its own projected finish.
	r.Projection = projectionLine(sess.Lang, h, r.RequiredHoursLow, r.RequiredHoursHigh)
	// A recap with nothing to read and nothing to answer would strand the
	// conversation, since the gate will not let a plan through without approval.
	if !r.Decision.Approved && strings.TrimSpace(r.Summary) == "" && strings.TrimSpace(r.Verdict) == "" && strings.TrimSpace(r.ReplyToUser) == "" {
		return feasibilityResult{}, fmt.Errorf("%w: confirm stage returned nothing to show the user", errAIUnavailable)
	}
	return r, nil
}

// applyConfirmOutcome records the agreed verdict on the session.
func applyConfirmOutcome(sess *IntakeSession, r feasibilityResult) {
	sess.FeasibilityNote = firstNonEmpty(r.Verdict, sess.FeasibilityNote)
	if s := normalizeFeasibilityStatus(r.Status); s != unknownStatus || sess.FeasibilityStatus == "" {
		sess.FeasibilityStatus = s
	}
}

// nextIntake runs one adaptive intake step, real or mock. In live mode every
// failure is reported: it never falls through to the mock brain.
//
// The SUBJECT of the question is chosen here, not by the model, and so is the
// decision to stop. Left to the model, a real run spent all six turns on
// motivation (twice), learning style and "what is your main focus", and never
// asked how many hours a week, which days, by when, or what the user could
// spend — so the plan was built on defaults the user never chose. The model
// still writes and localizes the question; it does not get to pick the topic.
func (p *Pipeline) nextIntake(ctx context.Context, sess *IntakeSession, latest string, opening bool) (intakeResult, error) {
	if !p.gw.Enabled() {
		// The mock brain maps a reply onto the next unanswered category, which
		// only means anything once it has asked something. On the opening turn it
		// has not, so the goal message is not an answer to it — only the live
		// model mines that message for level, target and availability.
		if opening {
			latest = ""
		}
		return mockIntake(sess, latest), nil
	}
	askAbout := nextIntakeCategory(sess)
	payload := p.buildIntakeContext(sess, askAbout)
	history := conversationWindow(sess)
	task := "Merge the current user message into the answers, then ask about the first outstanding category in askCategories. Do not choose a topic of your own."
	turns := stageTurns(prompts["intake"].System, sess.Lang, history, payload, latest, task)
	raw, err := p.gw.ChatTurns(ctx, sess.UserID, p.cfg.ModelFast, turns, true,
		cacheKeyForTurns("intake", sess.Lang, payload+"\x00"+latest, history))
	if err != nil {
		return intakeResult{}, err
	}
	var r intakeResult
	if e := json.Unmarshal([]byte(extractJSONObject(raw)), &r); e != nil {
		return intakeResult{}, fmt.Errorf("%w: intake stage returned unparsable JSON: %v", errAIUnavailable, e)
	}
	applyAnswers(sess, r.Answers)
	// The model reads language and intent; the backend does the arithmetic.
	// "Mon, Wed and Fri, an hour each" is exactly 180 minutes a week, and
	// deriving that here rather than trusting a model to add up three numbers
	// is what makes the availability — and therefore the whole feasibility
	// calculation — reproducible.
	if !r.LatestWasQuestion {
		recordStatedAvailability(sess, latest, askAbout, r.Answers)
	}
	if note := strings.TrimSpace(r.NoteForPlan); note != "" {
		sess.PlanNotes = appendNote(sess.PlanNotes, note)
	}

	// The user asked something instead of answering. Their question gets an
	// answer and the pending category stays pending — it is not marked, so the
	// interview resumes exactly where it was rather than losing that slot.
	if r.LatestWasQuestion && strings.TrimSpace(r.NextQuestion) != "" {
		r.Done = false
		return r, nil
	}

	// askAbout was chosen before this reply had been parsed. Now that it is
	// merged, re-derive the truth: an opening message that supplied everything
	// ("I'm A1, want C1 by next summer, 6 hours a week on Mon/Wed/Fri") ends the
	// interview here, whatever we were a moment ago about to ask.
	if nextIntakeCategory(sess) == "" {
		r.Done = true
		r.NextQuestion, r.Options = "", nil
		return r, nil
	}

	// A reply often volunteers more than it was asked for, so the model may
	// legitimately have skipped ahead; it reports which category it actually
	// covered. Anything it makes up is ignored in favour of what we asked for.
	asked := askAbout
	if isIntakeCategory(r.Asked) {
		asked = r.Asked
	}
	// Never mark a category that the merge has already filled — the question
	// was about something else, and marking it would burn a slot for nothing.
	if asked == "" || isCategoryFilled(sess, asked) {
		asked = nextIntakeCategory(sess)
	}
	markCategoryAsked(sess, asked)

	r.Done = false
	// A model that neither finishes nor asks anything would leave the user
	// staring at an empty bubble. That is a live failure to surface, not a cue
	// to silently hand the conversation to the mock question set.
	if strings.TrimSpace(r.NextQuestion) == "" {
		return intakeResult{}, fmt.Errorf("%w: intake stage returned no question for %q", errAIUnavailable, asked)
	}
	return r, nil
}

func applyAnswers(sess *IntakeSession, m map[string]string) {
	if m == nil {
		return
	}
	a := &sess.Answers
	set := func(dst *string, key string) {
		if v := strings.TrimSpace(m[key]); v != "" {
			*dst = v
		}
	}
	set(&a.CurrentLevel, "currentLevel")
	set(&a.Target, "target")
	set(&a.Budget, "budget")
	set(&a.Location, "location")
	set(&a.LearningStyle, "learningStyle")
	set(&a.Motivation, "motivation")
	set(&a.PivotalChoice, "pivotalChoice")
	if v := strings.TrimSpace(m["deadline"]); v != "" {
		if validDate(v) {
			a.Deadline = v
		} else if d := reISODate.FindString(v); d != "" && validDate(d) {
			a.Deadline = d
		}
	}
	// Availability is deliberately NOT applied here — see recordStatedAvailability.
}

// availabilityFromModel decodes the availability the model reported. It is a
// CLAIM, not a fact: nothing here reaches the session until the user's own
// words have been shown to contain an availability.
func availabilityFromModel(m map[string]string) StudyAvailability {
	var add StudyAvailability
	if m == nil {
		return add
	}
	if v := strings.TrimSpace(m["days"]); v != "" {
		add.Days = detectDays(v)
	}
	if v := strings.TrimSpace(m["hoursPerWeek"]); v != "" {
		if n, ok := parseHoursPerWeek(v); ok {
			add.WeeklyMinutes = n * 60
		}
	}
	if v := strings.TrimSpace(m["weeklyMinutes"]); v != "" {
		if n, ok := parseInt(v); ok && n > 0 {
			add.WeeklyMinutes = n
		}
	}
	// perDay arrives as "Mon:60,Wed:60,Fri:90" — the answerMap decoder has
	// already flattened whatever shape the model used into a scalar string.
	if v := strings.TrimSpace(m["perDay"]); v != "" {
		for _, part := range strings.Split(v, ",") {
			bits := strings.SplitN(strings.TrimSpace(part), ":", 2)
			if len(bits) != 2 {
				continue
			}
			codes := detectDays(bits[0])
			mins, ok := parseInt(bits[1])
			if len(codes) == 0 || !ok || mins <= 0 {
				continue
			}
			add.PerDay = append(add.PerDay, DayAvailability{Weekday: codes[0], Minutes: mins})
		}
	}
	add.normalize()
	return add
}

// recordStatedAvailability is the ONLY way an availability enters the session
// during the interview.
//
// The rule is: availability comes from the learner's own words. The model may
// finish a half those words left unreadable, but it may never supply one they
// never gave.
//
// This exists because the opposite failed in production. Asked for their level,
// a learner answered "i dont have knowledge"; the model helpfully filled in
// days=["Mon"], hoursPerWeek=2 alongside it. That completed the availability,
// so the interview never asked the question, the recap reported "2 hours per
// week on Mon" as though the learner had said it, and the plan was built and
// scheduled against a week they had never agreed to. Removing the backend's own
// silent default was not enough while the model could still invent one — and an
// invented availability is unrecoverable downstream, because nothing can tell it
// apart from a real answer.
//
// An answer the parser cannot read simply leaves availability unknown. That is
// recoverable: the interview asks again, and failing that the verdict honestly
// says "unknown".
func recordStatedAvailability(sess *IntakeSession, latest, askAbout string, modelAnswers map[string]string) {
	st := parseAvailabilityStatement(latest)
	if st.empty() {
		return // the learner said nothing about when they can study
	}
	// Resolved against what is already known, so a per-day rate lands on the
	// days already in force: "5h/day" to someone studying seven days is
	// thirty-five hours a week, and they should not have to restate their week
	// to change its length. The bare-number reading ("about 6") is only
	// trustworthy when we know this reply is answering that question.
	current := currentAvailability(sess)
	next := resolveAvailability(current, st, askAbout == "timeBudget")
	if availabilityFingerprint(next) == availabilityFingerprint(current) {
		return
	}

	// The model may complete a half the learner's words left unread — "a couple
	// of evenings after work" is a real answer the parser cannot structure — but
	// only a half that is genuinely still missing.
	if claim := availabilityFromModel(modelAnswers); claim.HasDays() || claim.HasTime() {
		var fill StudyAvailability
		if !next.HasDays() && claim.HasDays() {
			fill.Days = claim.Days
		}
		if !next.HasTime() && claim.HasTime() {
			fill.WeeklyMinutes = claim.WeeklyMinutes
			fill.PerDay = claim.PerDay
		}
		if fill.HasDays() || fill.HasTime() {
			next = mergeAvailability(next, fill)
		}
	}
	syncAnswersAvailability(sess, next)
	invalidateFeasibility(sess)
}

// invalidateFeasibility discards every verdict derived from the availability,
// deadline or target that has just changed.
//
// Feasibility is a DERIVED value. Keeping a verdict computed from the previous
// week is how a recap ended up showing thirty-five hours a week above a
// capacity figure calculated from two. Nothing here is recomputed eagerly: the
// next recap recomputes from the state as it now stands, which is the only
// version anyone should be asked to approve.
func invalidateFeasibility(sess *IntakeSession) {
	sess.FeasibilityNote = ""
	sess.FeasibilityStatus = ""
	sess.FeasibilityAgreed = false
}

func (p *Pipeline) finishIntakeAndPlan(ctx context.Context, sess *IntakeSession) (Turn, error) {
	plan, err := p.buildPlan(ctx, sess)
	if err != nil {
		return Turn{}, err
	}
	p.store.SavePlan(plan)
	sess.PlanID = plan.ID
	sess.Stage = "plan_ready"

	// Remember the availability on the profile so a second goal does not have
	// to ask for it again.
	p.store.UpdateUserAvailability(sess.UserID, plan.HoursPerWeek, plan.Days)

	msg := tr(sess.Lang,
		"All set — I built your plan for "+plan.Skill+". "+plan.Assessment+"\n\nOpen the plan on the right, then hit “Schedule it” to lay it on your calendar.",
		"Готово — я составил ваш план для «"+plan.Skill+"». "+plan.Assessment+"\n\nОткройте план справа и нажмите «Запланировать», чтобы разложить его по календарю.",
		"Tayyor — men «"+plan.Skill+"» uchun rejangizni tuzdim. "+plan.Assessment+"\n\nO'ngdagi rejani oching va uni kalendaringizga joylash uchun «Rejaga qo'yish» tugmasini bosing.")
	return Turn{Stage: "plan_ready", Assistant: msg, PlanID: plan.ID, Done: true}, nil
}

// buildPlan produces the plan (real AI or mock) and materializes it into models.
// In live mode a failure is returned, never papered over with a mock plan: a
// user must not be handed canned content believing the model produced it.
func (p *Pipeline) buildPlan(ctx context.Context, sess *IntakeSession) (*Plan, error) {
	tz := ""
	if u, ok := p.store.GetUser(sess.UserID); ok {
		tz = u.Timezone
	}
	loc := loadLocation(tz)

	var pa planAI
	if p.gw.Enabled() {
		payload := p.buildPlanContext(sess, loc)
		history := conversationWindow(sess)
		turns := stageTurns(prompts["plan"].System, sess.Lang, history, payload, "",
			"Build the learning plan for the agreed target, inside the stated weekly minute budget.")
		raw, err := p.gw.ChatTurns(ctx, sess.UserID, p.cfg.ModelSmart, turns, true,
			cacheKeyForTurns("plan", sess.Lang, payload, history))
		if err != nil {
			return nil, err
		}
		if e := json.Unmarshal([]byte(extractJSONObject(raw)), &pa); e != nil {
			return nil, fmt.Errorf("%w: plan stage returned unparsable JSON: %v", errAIUnavailable, e)
		}
		if len(pa.Todos) == 0 {
			return nil, fmt.Errorf("%w: plan stage returned no todos", errAIUnavailable)
		}
	} else {
		pa = mockPlan(sess, loc)
	}
	return materializePlan(sess, pa, tz), nil
}

// materializePlan turns the AI/mock plan shape into the stored domain model,
// assigning IDs, validating week ranges, and computing milestone dates from
// week offsets.
func materializePlan(sess *IntakeSession, pa planAI, timezone string) *Plan {
	loc := loadLocation(timezone)
	weeks := clamp(pa.WeeksTotal, 1, maxPlanWeeks)
	av, _ := planAvailability(sess)
	start := todayIn(loc).AddDate(0, 0, 1)

	plan := &Plan{
		ID:                newID("plan"),
		UserID:            sess.UserID,
		GoalID:            sess.GoalID,
		Skill:             sess.Skill,
		Path:              sess.Path,
		Assessment:        pa.Assessment,
		Feasibility:       pa.Feasibility,
		WeeksTotal:        weeks,
		Timezone:          timezone,
		Lang:              normLang(sess.Lang),
		Deadline:          sess.Answers.Deadline,
		FeasibilityStatus: firstNonEmpty(sess.FeasibilityStatus, unknownStatus),
		StartDate:         dateStr(start),
		Version:           1,
		CreatedAt:         time.Now(),
	}
	// One writer for the availability fields, so Days, HoursPerWeek,
	// WeeklyMinutes and PerDay can never disagree with each other.
	syncPlanAvailability(plan, av)

	// Week ranges arriving from the model (or from a short mock plan) are
	// clamped into the plan, and an inverted range is repaired rather than
	// handed to the scheduler.
	for _, ph := range pa.Phases {
		ws := clamp(ph.WeekStart, 1, weeks)
		we := clamp(ph.WeekEnd, 1, weeks)
		if we < ws {
			we = ws
		}
		plan.Phases = append(plan.Phases, Phase{
			Key: ph.Key, Title: ph.Title, Summary: ph.Summary,
			WeekStart: ws, WeekEnd: we,
		})
	}
	for _, m := range pa.Milestones {
		tw := clamp(m.TargetWeek, 1, weeks)
		plan.Milestones = append(plan.Milestones, Milestone{
			ID:         newID("ms"),
			Title:      m.Title,
			Phase:      m.Phase,
			TargetDate: dateStr(start.AddDate(0, 0, tw*7)),
		})
	}
	for _, t := range pa.Todos {
		dur := t.DurationMin
		if dur <= 0 {
			dur = 45
		}
		plan.Todos = append(plan.Todos, Todo{
			ID:          newID("todo"),
			PlanID:      plan.ID,
			Title:       t.Title,
			DurationMin: clamp(dur, 5, 8*60),
			Frequency:   normalizeFrequency(t.Frequency),
			Priority:    normalizePriority(t.Priority),
			Phase:       t.Phase,
			DependsOn:   t.DependsOn,
			ResourceRef: t.ResourceRef,
			Status:      "pending",
		})
	}
	for _, s := range pa.SetupItems {
		plan.SetupItems = append(plan.SetupItems, SetupItem{
			ID:          newID("item"),
			PlanID:      plan.ID,
			Name:        s.Name,
			Category:    s.Category,
			Priority:    normalizePriority(s.Priority),
			PriceRange:  s.PriceRange,
			Owned:       s.Owned,
			Rationale:   s.Rationale,
			SearchQuery: cleanShopQuery(s.SearchQuery),
		})
	}
	// Record what was recommended, so a later "I bought a different one" has
	// something concrete to replace.
	seedResources(plan)
	return plan
}

func normalizeFrequency(f string) string {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "once", "weekly", "twice_weekly", "thrice_weekly", "daily":
		return strings.ToLower(strings.TrimSpace(f))
	default:
		return "weekly"
	}
}

func normalizePriority(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "high", "medium", "low":
		return strings.ToLower(strings.TrimSpace(p))
	default:
		return "medium"
	}
}

// isCategoryFilled reports whether one category already has its answer.
func isCategoryFilled(sess *IntakeSession, key string) bool {
	for _, c := range intakeCategories {
		if c.key == key {
			return c.filled(sess)
		}
	}
	return false
}

// appendNote adds a plan request, skipping one already recorded so a user who
// repeats themselves does not stack the same instruction three times.
func appendNote(notes []string, note string) []string {
	for _, n := range notes {
		if strings.EqualFold(strings.TrimSpace(n), note) {
			return notes
		}
	}
	return append(notes, note)
}

// withReply puts the answer to the user's own question above the interview
// question, so one bubble both answers them and keeps the interview moving.
func withReply(reply, question string) string {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return question
	}
	return reply + "\n\n" + question
}

// answersFingerprint captures every field the confirmation recap is computed
// from, so a change to any of them forces a fresh recap.
//
// It uses the full availability fingerprint rather than the rounded
// HoursPerWeek mirror: a change from "2 hours on seven days" to "2 hours on
// three days" leaves the mirror untouched while halving the capacity, and a
// recap that did not notice would show new days above an old total.
func answersFingerprint(sess *IntakeSession) string {
	a := sess.Answers
	return strings.Join([]string{
		a.Target, a.Deadline, a.CurrentLevel,
		availabilityFingerprint(currentAvailability(sess)),
	}, "|")
}
