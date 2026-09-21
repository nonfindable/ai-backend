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

type feasibilityResult struct {
	RequiredHours  int                 `json:"requiredHours"`
	AvailableHours int                 `json:"availableHours"`
	Reachable      bool                `json:"reachable"`
	Summary        string              `json:"summary"`
	Verdict        string              `json:"verdict"`
	Question       string              `json:"question"`
	Options        []feasibilityOption `json:"options"`
	Decision       feasibilityDecision `json:"decision"`

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
}

func newPipeline(cfg Config, store *Store, gw *Gateway, sched *Scheduler) *Pipeline {
	return &Pipeline{cfg: cfg, store: store, gw: gw, sched: sched}
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
}

var intakeCategories = []intakeCategory{
	{"pivotalChoice", func(s *IntakeSession) bool {
		// Only a real category when the understand stage found a fork to resolve.
		return s.PivotalChoice == "" || s.Answers.PivotalChoice != ""
	}},
	{"currentLevel", func(s *IntakeSession) bool { return s.Answers.CurrentLevel != "" }},
	{"target", func(s *IntakeSession) bool { return s.Answers.Target != "" }},
	{"timeBudget", func(s *IntakeSession) bool {
		// Hours and days are one question: "5 hours, weekends" answers both, and
		// splitting them wastes a turn on half an availability.
		return s.Answers.HoursPerWeek > 0 && len(s.Answers.Days) > 0
	}},
	{"deadline", func(s *IntakeSession) bool { return s.Answers.Deadline != "" }},
	{"budget", func(s *IntakeSession) bool { return s.Answers.Budget != "" }},
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

// nextIntakeCategory returns what the next question must cover, or "" when the
// interview is over. A category already put to the user is never raised again
// even if it came back empty: "no deadline in mind" is a complete answer, and
// re-asking would spend the whole interview on one field.
func nextIntakeCategory(sess *IntakeSession) string {
	for _, c := range intakeCategories {
		if c.filled(sess) || sess.AnswerBag[askedCategoryKey(c.key)] == "yes" {
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
	"timeBudget":    "how many hours per week AND which days of the week — one question covering both",
	"deadline":      "the date they want to be done by",
	"budget":        "how much money they can put into this",
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
		if !seen || c.filled(sess) || sess.AnswerBag[askedCategoryKey(c.key)] == "yes" {
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
	sess.AnswerBag[askedCategoryKey(key)] = "yes"
}

// ---- prompt context ----
//
// Every stage used to see one bare string, which left the model unable to do
// things the prompts ask of it: resolve "in three months" without today's
// date, avoid re-asking a question it could not see it had asked, or size a
// week of work against a budget it was never told. These builders assemble the
// JSON payload each stage documents, and because the gateway keys its cache on
// that payload, richer context also means a correctly narrower cache.

// promptHistoryTurns caps how much transcript a stage receives: enough for the
// model to recall what it already asked, short enough to keep both the token
// bill and the cache key bounded. Four is two exchanges — which is all the
// "did I already ask this?" checks need, since the categories the interview
// still owes are passed explicitly. Every extra turn is paid for on every
// call, and the daily token allowance is what a long conversation runs out of
// first.
const promptHistoryTurns = 4

type promptMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// recentTurns renders the tail of the conversation, oldest first. The latest
// user message is already appended by HandleChat, so it appears here as well as
// in the stage's own "latest" field — a duplicate costs a few tokens and is far
// safer than an off-by-one that hides the message the model must answer.
func recentTurns(sess *IntakeSession) []promptMessage {
	msgs := sess.Messages
	if len(msgs) > promptHistoryTurns {
		msgs = msgs[len(msgs)-promptHistoryTurns:]
	}
	out := make([]promptMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, promptMessage{Role: m.Role, Content: m.Content})
	}
	return out
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

// planAvailability resolves the weekly budget the plan is built against. The
// plan prompt and materializePlan must agree on it exactly: telling the model a
// budget the scheduler then ignores is how todos end up silently dropped.
func planAvailability(sess *IntakeSession) (hours int, days []string) {
	hours = sess.Answers.HoursPerWeek
	if hours <= 0 {
		hours = defaultHoursWeek
	}
	days = sess.Answers.Days
	if len(days) == 0 {
		days = []string{"Mon", "Wed", "Fri"}
	}
	return hours, days
}

func (p *Pipeline) buildUnderstandContext(sess *IntakeSession, msg string) string {
	_, loc := p.sessionUser(sess)
	b, _ := json.Marshal(map[string]any{
		"today":          dateStr(todayIn(loc)),
		"latestMessage":  msg,
		"recentMessages": recentTurns(sess),
		"knownSkill":     sess.Skill,
	})
	return string(b)
}

func (p *Pipeline) buildIntakeContext(sess *IntakeSession, latest, askAbout string) string {
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
	b, _ := json.Marshal(map[string]any{
		"today":         dateStr(todayIn(loc)),
		"skill":         sess.Skill,
		"path":          sess.Path,
		"pivotalChoice": sess.PivotalChoice,
		"knownAnswers":  sess.Answers,
		"profile":       profile,
		// The backend's choice of subject for this turn, and what is still
		// outstanding after it, so a reply that answers ahead can skip forward.
		"askAbout":        askAbout,
		"askCategories":   append([]string{askAbout}, remainingCategories(sess, askAbout)...),
		"categoryMeaning": intakeCategoryMeaning,
		// The interview is cut off at maxIntakeQuestions whatever the model
		// wants, so it has to know how much room is left to spend.
		"questionsAsked":     sess.AskedCount,
		"questionsRemaining": maxInt(0, maxIntakeQuestions-sess.AskedCount),
		"recentMessages":     recentTurns(sess),
		"latestReply":        latest,
	})
	return string(b)
}

// planHorizon resolves the schedule arithmetic both the feasibility gate and
// the plan stage reason about, from one place so the two can never disagree
// about how much time the user actually has.
func planHorizon(sess *IntakeSession, loc *time.Location) (start time.Time, hours int, days []string, weeksUntilDeadline int) {
	hours, days = planAvailability(sess)
	start = todayIn(loc).AddDate(0, 0, 1) // same first day materializePlan uses
	if d, ok := parseDateIn(sess.Answers.Deadline, loc); ok {
		if n := daysBetween(start, d); n > 0 {
			weeksUntilDeadline = (n + 6) / 7 // whole weeks, rounded up
		}
	}
	return start, hours, days, weeksUntilDeadline
}

func (p *Pipeline) buildFeasibilityContext(sess *IntakeSession, loc *time.Location, latest string) string {
	_, hours, days, weeksUntilDeadline := planHorizon(sess, loc)
	b, _ := json.Marshal(map[string]any{
		"today":               dateStr(todayIn(loc)),
		"skill":               sess.Skill,
		"path":                sess.Path,
		"currentLevel":        sess.Answers.CurrentLevel,
		"target":              sess.Answers.Target,
		"deadline":            sess.Answers.Deadline,
		"hoursPerWeek":        hours,
		"days":                days,
		"weeksUntilDeadline":  weeksUntilDeadline,
		"totalHoursAvailable": weeksUntilDeadline * hours,
		"maxWeeks":            maxPlanWeeks,
		// The full answer set, so the recap can be checked against everything
		// the user actually said rather than the handful of scheduling fields.
		"answers":        sess.Answers,
		"planNotes":      sess.PlanNotes,
		"recentMessages": recentTurns(sess),
		"latestReply":    latest,
	})
	return string(b)
}

func (p *Pipeline) buildPlanContext(sess *IntakeSession, loc *time.Location) string {
	start, hours, days, weeksUntilDeadline := planHorizon(sess, loc)

	b, _ := json.Marshal(map[string]any{
		"today":              dateStr(todayIn(loc)),
		"startDate":          dateStr(start),
		"skill":              sess.Skill,
		"path":               sess.Path,
		"answers":            sess.Answers,
		"weeklyMinuteBudget": hours * 60,
		"hoursPerWeek":       hours,
		"days":               days,
		"daysAvailable":      len(days),
		"deadline":           sess.Answers.Deadline,
		"weeksUntilDeadline": weeksUntilDeadline,
		// The total study hours the deadline actually buys. Handing the model
		// the finished number is what lets it check a target against reality
		// instead of calling every timeline "achievable". 0 means no deadline.
		"totalHoursAvailable": weeksUntilDeadline * hours,
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
	sess.Messages = append(sess.Messages, Message{Role: "user", Content: userMsg, At: time.Now()})
	if sess.Stage == "" {
		sess.Stage = "scope_check"
	}

	var turn Turn
	var err error
	switch sess.Stage {
	case "scope_check", "out_of_scope":
		turn, err = p.doUnderstand(ctx, sess, userMsg)
	case "disambiguation":
		turn, err = p.doDisambiguation(ctx, sess, userMsg)
	case "intake":
		turn, err = p.doIntake(ctx, sess, userMsg)
	case "confirm_plan":
		turn, err = p.doConfirm(ctx, sess, userMsg)
	case "plan_ready":
		turn = Turn{Stage: sess.Stage, PlanID: sess.PlanID, Assistant: tr(sess.Lang,
			"Your plan is ready — open it on the right, or say a new goal to start another.",
			"Ваш план готов — откройте его справа или назовите новую цель, чтобы начать другой.",
			"Rejangiz tayyor — uni o'ng tomondan oching yoki boshqasini boshlash uchun yangi maqsad ayting.")}
	default:
		turn, err = p.doUnderstand(ctx, sess, userMsg)
	}
	if err != nil {
		return Turn{}, err
	}

	turn.SessionID = sess.ID
	if turn.Assistant != "" {
		sess.Messages = append(sess.Messages, Message{Role: "assistant", Content: turn.Assistant, At: time.Now()})
	}
	p.store.SaveSession(sess)
	return turn, nil
}

func (p *Pipeline) doUnderstand(ctx context.Context, sess *IntakeSession, msg string) (Turn, error) {
	var u understandResult
	if p.gw.Enabled() {
		payload := p.buildUnderstandContext(sess, msg)
		raw, err := p.gw.Chat(ctx, sess.UserID, p.cfg.ModelFast, withLang(prompts["understand"].System, sess.Lang), payload, true, cacheKeyFor("understand", sess.Lang, payload))
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
func (p *Pipeline) gateBeforePlan(ctx context.Context, sess *IntakeSession) (Turn, error) {
	if sess.FeasibilityAgreed {
		return p.finishIntakeAndPlan(ctx, sess)
	}
	r, err := p.confirmBeforePlan(ctx, sess, "")
	if err != nil {
		return Turn{}, err
	}
	sess.FeasibilityNote = r.Verdict
	sess.Stage = "confirm_plan"
	return confirmTurn(sess, r), nil
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
	sess.FeasibilityNote = firstNonEmpty(r.Verdict, sess.FeasibilityNote)

	// An adjustment they asked for is applied whether or not they also approved,
	// so "make it 10 hours a week, go ahead" does both in one message.
	changed := false
	if r.Decision.Resolved && !r.Decision.KeepOriginal {
		before := answersFingerprint(sess)
		applyFeasibilityDecision(sess, r.Decision)
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
			sess.FeasibilityNote = firstNonEmpty(fresh.Verdict, sess.FeasibilityNote)
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
	for _, s := range []string{r.ReplyToUser, r.Summary, r.Verdict, question} {
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
func applyFeasibilityDecision(sess *IntakeSession, d feasibilityDecision) {
	a := &sess.Answers
	if v := strings.TrimSpace(d.Target); v != "" {
		a.Target = v
	}
	if v := strings.TrimSpace(d.Deadline); validDate(v) {
		a.Deadline = v
	}
	if d.HoursPerWeek > 0 {
		a.HoursPerWeek = clamp(d.HoursPerWeek, 1, 40)
	}
	// Run through detectDays exactly as applyAnswers does, so "Monday, Tuesday
	// and Friday" becomes the canonical codes the scheduler matches on and a
	// name it cannot parse changes nothing rather than emptying the week.
	if len(d.Days) > 0 {
		if days := detectDays(strings.Join(d.Days, ",")); len(days) > 0 {
			a.Days = days
		}
	}
}

func (p *Pipeline) confirmBeforePlan(ctx context.Context, sess *IntakeSession, latest string) (feasibilityResult, error) {
	_, loc := p.sessionUser(sess)
	if !p.gw.Enabled() {
		_, hours, _, weeks := planHorizon(sess, loc)
		return mockConfirm(sess, weeks*hours, latest), nil
	}
	payload := p.buildFeasibilityContext(sess, loc, latest)
	raw, err := p.gw.Chat(ctx, sess.UserID, p.cfg.ModelSmart, withLang(prompts["confirm"].System, sess.Lang), payload, true, cacheKeyFor("confirm", sess.Lang, payload))
	if err != nil {
		return feasibilityResult{}, err
	}
	var r feasibilityResult
	if e := json.Unmarshal([]byte(extractJSONObject(raw)), &r); e != nil {
		return feasibilityResult{}, fmt.Errorf("%w: confirm stage returned unparsable JSON: %v", errAIUnavailable, e)
	}
	// A recap with nothing to read and nothing to answer would strand the
	// conversation, since the gate will not let a plan through without approval.
	if !r.Decision.Approved && strings.TrimSpace(r.Summary) == "" && strings.TrimSpace(r.Verdict) == "" && strings.TrimSpace(r.ReplyToUser) == "" {
		return feasibilityResult{}, fmt.Errorf("%w: confirm stage returned nothing to show the user", errAIUnavailable)
	}
	return r, nil
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
	payload := p.buildIntakeContext(sess, latest, askAbout)
	raw, err := p.gw.Chat(ctx, sess.UserID, p.cfg.ModelFast, withLang(prompts["intake"].System, sess.Lang), payload, true, cacheKeyFor("intake", sess.Lang, payload))
	if err != nil {
		return intakeResult{}, err
	}
	var r intakeResult
	if e := json.Unmarshal([]byte(extractJSONObject(raw)), &r); e != nil {
		return intakeResult{}, fmt.Errorf("%w: intake stage returned unparsable JSON: %v", errAIUnavailable, e)
	}
	applyAnswers(sess, r.Answers)
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
	if v := strings.TrimSpace(m["hoursPerWeek"]); v != "" {
		if n, ok := parseHoursPerWeek(v); ok {
			a.HoursPerWeek = n
		}
	}
	if v := strings.TrimSpace(m["days"]); v != "" {
		if days := detectDays(v); len(days) > 0 {
			a.Days = days
		}
	}
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
		raw, err := p.gw.Chat(ctx, sess.UserID, p.cfg.ModelSmart, withLang(prompts["plan"].System, sess.Lang), payload, true, cacheKeyFor("plan", sess.Lang, payload))
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
	hours, days := planAvailability(sess)
	start := todayIn(loc).AddDate(0, 0, 1)

	plan := &Plan{
		ID:           newID("plan"),
		UserID:       sess.UserID,
		GoalID:       sess.GoalID,
		Skill:        sess.Skill,
		Path:         sess.Path,
		Assessment:   pa.Assessment,
		Feasibility:  pa.Feasibility,
		HoursPerWeek: hours,
		Days:         days,
		WeeksTotal:   weeks,
		Timezone:     timezone,
		Lang:         normLang(sess.Lang),
		Deadline:     sess.Answers.Deadline,
		StartDate:    dateStr(start),
		Version:      1,
		CreatedAt:    time.Now(),
	}

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
			ID:         newID("item"),
			PlanID:     plan.ID,
			Name:       s.Name,
			Category:   s.Category,
			Priority:   normalizePriority(s.Priority),
			PriceRange: s.PriceRange,
			Owned:      s.Owned,
			Rationale:  s.Rationale,
		})
	}
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

// answersFingerprint captures the fields the confirmation recap is computed
// from, so a change to any of them triggers a fresh recap.
func answersFingerprint(sess *IntakeSession) string {
	a := sess.Answers
	return strings.Join([]string{a.Target, a.Deadline, itoa(a.HoursPerWeek), strings.Join(a.Days, ",")}, "|")
}
