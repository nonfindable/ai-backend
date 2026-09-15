package main

import "time"

// User — profile plus the reusable availability that skills should not re-ask.
type User struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Timezone     string    `json:"timezone"`
	HoursPerWeek int       `json:"hoursPerWeek"`
	Days         []string  `json:"days"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (u *User) clone() *User {
	if u == nil {
		return nil
	}
	c := *u
	c.Days = append([]string(nil), u.Days...)
	return &c
}

// FrameworkAnswers — the universal intake categories, specialized per skill.
type FrameworkAnswers struct {
	CurrentLevel  string   `json:"currentLevel"`
	Target        string   `json:"target"`
	Deadline      string   `json:"deadline"` // YYYY-MM-DD or free text
	HoursPerWeek  int      `json:"hoursPerWeek"`
	Days          []string `json:"days"`
	Budget        string   `json:"budget"`
	Location      string   `json:"location"`
	LearningStyle string   `json:"learningStyle"`
	Motivation    string   `json:"motivation"`
	PivotalChoice string   `json:"pivotalChoice"` // e.g. Academic vs General
}

type Goal struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	RawInput  string    `json:"rawInput"`
	Skill     string    `json:"skill"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"createdAt"`
}

func (g *Goal) clone() *Goal {
	if g == nil {
		return nil
	}
	c := *g
	return &c
}

type Message struct {
	Role    string    `json:"role"` // user | assistant
	Content string    `json:"content"`
	At      time.Time `json:"at"`
}

// IntakeSession — the running state machine for one goal conversation.
type IntakeSession struct {
	ID                  string            `json:"id"`
	UserID              string            `json:"userId"`
	GoalID              string            `json:"goalId"`
	Stage               string            `json:"stage"` // scope_check|disambiguation|intake|plan_ready|out_of_scope
	Lang                string            `json:"lang"`  // en|ru|uz
	Skill               string            `json:"skill"`
	Path                string            `json:"path"`
	Overview            string            `json:"overview"`
	PivotalChoice       string            `json:"pivotalChoice"`
	NeedsDisambiguation bool              `json:"needsDisambiguation"`
	Messages            []Message         `json:"messages"`
	Answers             FrameworkAnswers  `json:"answers"`
	AnswerBag           map[string]string `json:"answerBag"` // raw answers keyed by category
	AskedCount          int               `json:"askedCount"`
	PlanID              string            `json:"planId"`
	CreatedAt           time.Time         `json:"createdAt"`
	UpdatedAt           time.Time         `json:"updatedAt"`
}

func (s *IntakeSession) clone() *IntakeSession {
	if s == nil {
		return nil
	}
	c := *s
	c.Messages = append([]Message(nil), s.Messages...)
	c.Answers.Days = append([]string(nil), s.Answers.Days...)
	if s.AnswerBag != nil {
		c.AnswerBag = make(map[string]string, len(s.AnswerBag))
		for k, v := range s.AnswerBag {
			c.AnswerBag[k] = v
		}
	}
	return &c
}

type Todo struct {
	ID          string     `json:"id"`
	PlanID      string     `json:"planId"`
	Title       string     `json:"title"`
	DurationMin int        `json:"durationMin"`
	Frequency   string     `json:"frequency"` // once|weekly|twice_weekly|thrice_weekly|daily
	Priority    string     `json:"priority"`  // high|medium|low
	Phase       string     `json:"phase"`
	DependsOn   []string   `json:"dependsOn"`
	ResourceRef string     `json:"resourceRef"`
	Status      string     `json:"status"` // pending|in_progress|done|skipped
	CompletedAt *time.Time `json:"completedAt,omitempty"`

	// A todo is a recurring commitment, not a single checkbox: it is scheduled
	// as PlannedCount separate sessions and only becomes "done" once every one
	// of them is completed. Tracking this per occurrence is what stops a single
	// completed session from cancelling the whole series.
	PlannedCount   int `json:"plannedCount"`
	CompletedCount int `json:"completedCount"`
}

func (t Todo) clone() Todo {
	c := t
	c.DependsOn = append([]string(nil), t.DependsOn...)
	if t.CompletedAt != nil {
		at := *t.CompletedAt
		c.CompletedAt = &at
	}
	return c
}

// Remaining reports how many sessions of this todo are still outstanding.
func (t Todo) Remaining() int {
	if t.Status == "skipped" {
		return 0
	}
	if t.PlannedCount <= 0 {
		if t.Status == "done" {
			return 0
		}
		return 1
	}
	return maxInt(0, t.PlannedCount-t.CompletedCount)
}

type Milestone struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Phase      string `json:"phase"`
	TargetDate string `json:"targetDate"`
	Done       bool   `json:"done"`
}

type Phase struct {
	Key       string `json:"key"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	WeekStart int    `json:"weekStart"`
	WeekEnd   int    `json:"weekEnd"`
}

type SetupItem struct {
	ID         string `json:"id"`
	PlanID     string `json:"planId"`
	Name       string `json:"name"`
	Category   string `json:"category"`
	Priority   string `json:"priority"`
	PriceRange string `json:"priceRange"`
	Owned      bool   `json:"owned"`
	Rationale  string `json:"rationale"`
}

// Plan — versioned; phases + milestones + assessment, with sized todos underneath.
type Plan struct {
	ID                 string      `json:"id"`
	UserID             string      `json:"userId"`
	GoalID             string      `json:"goalId"`
	Skill              string      `json:"skill"`
	Path               string      `json:"path"`
	Assessment         string      `json:"assessment"`
	Feasibility        string      `json:"feasibility"`
	Phases             []Phase     `json:"phases"`
	Milestones         []Milestone `json:"milestones"`
	Todos              []Todo      `json:"todos"`
	SetupItems         []SetupItem `json:"setupItems"`
	HoursPerWeek       int         `json:"hoursPerWeek"`
	Days               []string    `json:"days"`
	WeeksTotal         int         `json:"weeksTotal"`
	Timezone           string      `json:"timezone"`
	Lang               string      `json:"lang"`
	StartDate          string      `json:"startDate"`
	FinishDate         string      `json:"finishDate"`
	OriginalFinishDate string      `json:"originalFinishDate"`

	// Deadline is the user's hard date, carried through from intake so the
	// scheduler can check the plan against it instead of only using it to pick
	// a plan length.
	Deadline         string `json:"deadline,omitempty"`
	MissesDeadline   bool   `json:"missesDeadline"`
	DeadlineSlipDays int    `json:"deadlineSlipDays,omitempty"`
	DeadlineNote     string `json:"deadlineNote,omitempty"`

	// DroppedSessions counts occurrences the weekly time budget could not fit,
	// so a shortfall is reported rather than silently truncated away.
	DroppedSessions int `json:"droppedSessions"`

	Version   int       `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (p *Plan) clone() *Plan {
	if p == nil {
		return nil
	}
	c := *p
	c.Phases = append([]Phase(nil), p.Phases...)
	c.Milestones = append([]Milestone(nil), p.Milestones...)
	c.SetupItems = append([]SetupItem(nil), p.SetupItems...)
	c.Days = append([]string(nil), p.Days...)
	if p.Todos != nil {
		c.Todos = make([]Todo, len(p.Todos))
		for i, t := range p.Todos {
			c.Todos[i] = t.clone()
		}
	}
	return &c
}

// CalendarEvent — a scheduled todo instance on start.ai's own web calendar.
type CalendarEvent struct {
	ID           string `json:"id"`
	UserID       string `json:"userId"`
	PlanID       string `json:"planId"`
	TodoID       string `json:"todoId"`
	Title        string `json:"title"`
	Date         string `json:"date"` // YYYY-MM-DD
	StartTime    string `json:"startTime"`
	DurationMin  int    `json:"durationMin"`
	ReminderMin  int    `json:"reminderMin"`
	Status       string `json:"status"`       // proposed|scheduled|done|skipped
	ExportTarget string `json:"exportTarget"` // web_calendar (app version would be device_calendar)
	RolledOver   int    `json:"rolledOver"`   // how many times this session has slipped
}

func (e *CalendarEvent) clone() *CalendarEvent {
	if e == nil {
		return nil
	}
	c := *e
	return &c
}

type ProgressLog struct {
	ID     string    `json:"id"`
	UserID string    `json:"userId"`
	PlanID string    `json:"planId"`
	TodoID string    `json:"todoId"`
	Event  string    `json:"event"` // completed|skipped|rolled_over|milestone_hit
	At     time.Time `json:"at"`
	Note   string    `json:"note"`
}

func (p *ProgressLog) clone() *ProgressLog {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}
