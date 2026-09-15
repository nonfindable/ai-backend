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
	Answers      map[string]string `json:"answers"`
	NextQuestion string            `json:"nextQuestion"`
	Options      []string          `json:"options"`
	Done         bool              `json:"done"`
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
	Title       string   `json:"title"`
	DurationMin int      `json:"durationMin"`
	Frequency   string   `json:"frequency"`
	Priority    string   `json:"priority"`
	Phase       string   `json:"phase"`
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

type Pipeline struct {
	cfg   Config
	store *Store
	gw    *Gateway
	sched *Scheduler
}

func newPipeline(cfg Config, store *Store, gw *Gateway, sched *Scheduler) *Pipeline {
	return &Pipeline{cfg: cfg, store: store, gw: gw, sched: sched}
}

// maxIntakeQuestions bounds the interview so it always terminates.
const maxIntakeQuestions = 6

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
		raw, err := p.gw.Chat(ctx, sess.UserID, p.cfg.ModelFast, withLang(prompts["understand"].System, sess.Lang), msg, true, cacheKeyFor("understand", sess.Lang, msg))
		if err != nil {
			return Turn{}, err
		}
		// A malformed live response is a live failure. Quietly substituting the
		// mock brain here made a broken model indistinguishable from a working
		// one, in production as well as in demos.
		if e := json.Unmarshal([]byte(raw), &u); e != nil {
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
	first, err := p.firstIntakeTurn(ctx, sess)
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
	return p.firstIntakeTurn(ctx, sess)
}

func (p *Pipeline) firstIntakeTurn(ctx context.Context, sess *IntakeSession) (Turn, error) {
	r, err := p.nextIntake(ctx, sess, "")
	if err != nil {
		return Turn{}, err
	}
	lead := tr(sess.Lang,
		"Let's tailor your plan for "+sess.Skill+". ",
		"Давайте настроим ваш план для «"+sess.Skill+"». ",
		"Keling, «"+sess.Skill+"» uchun rejangizni moslaymiz. ")
	return p.intakeTurn(sess, lead+r.NextQuestion, r.NextQuestion, r.Options), nil
}

func (p *Pipeline) doIntake(ctx context.Context, sess *IntakeSession, msg string) (Turn, error) {
	sess.AskedCount++
	r, err := p.nextIntake(ctx, sess, msg)
	if err != nil {
		return Turn{}, err
	}

	if r.Done || sess.AskedCount >= maxIntakeQuestions {
		return p.finishIntakeAndPlan(ctx, sess)
	}
	return p.intakeTurn(sess, r.NextQuestion, r.NextQuestion, r.Options), nil
}

// nextIntake runs one adaptive intake step, real or mock. In live mode every
// failure is reported: it never falls through to the mock brain.
func (p *Pipeline) nextIntake(ctx context.Context, sess *IntakeSession, latest string) (intakeResult, error) {
	if !p.gw.Enabled() {
		return mockIntake(sess, latest), nil
	}
	// Build a compact context of skill + answers + latest reply.
	ctxObj := map[string]any{
		"skill":         sess.Skill,
		"path":          sess.Path,
		"pivotalChoice": sess.PivotalChoice,
		"knownAnswers":  sess.Answers,
		"latestReply":   latest,
	}
	b, _ := json.Marshal(ctxObj)
	raw, err := p.gw.Chat(ctx, sess.UserID, p.cfg.ModelFast, withLang(prompts["intake"].System, sess.Lang), string(b), true, cacheKeyFor("intake", sess.Lang, string(b)))
	if err != nil {
		return intakeResult{}, err
	}
	var r intakeResult
	if e := json.Unmarshal([]byte(raw), &r); e != nil {
		return intakeResult{}, fmt.Errorf("%w: intake stage returned unparsable JSON: %v", errAIUnavailable, e)
	}
	applyAnswers(sess, r.Answers)
	// A model that neither finishes nor asks anything would leave the user
	// staring at an empty bubble. That is a live failure to surface, not a cue
	// to silently hand the conversation to the mock question set.
	if !r.Done && strings.TrimSpace(r.NextQuestion) == "" {
		return intakeResult{}, fmt.Errorf("%w: intake stage returned neither a question nor done", errAIUnavailable)
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
		ctxObj := map[string]any{"skill": sess.Skill, "path": sess.Path, "answers": sess.Answers}
		b, _ := json.Marshal(ctxObj)
		raw, err := p.gw.Chat(ctx, sess.UserID, p.cfg.ModelSmart, withLang(prompts["plan"].System, sess.Lang), string(b), true, cacheKeyFor("plan", sess.Lang, string(b)))
		if err != nil {
			return nil, err
		}
		if e := json.Unmarshal([]byte(raw), &pa); e != nil {
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
	hours := sess.Answers.HoursPerWeek
	if hours <= 0 {
		hours = defaultHoursWeek
	}
	days := sess.Answers.Days
	if len(days) == 0 {
		days = []string{"Mon", "Wed", "Fri"}
	}
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
