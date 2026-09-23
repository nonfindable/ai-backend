package main

import (
	"reflect"
	"testing"
	"time"
)

func TestDetectDaysIsMultilingualAndOrdered(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"mon wed fri", []string{"Mon", "Wed", "Fri"}},
		{"fri wed mon", []string{"Mon", "Wed", "Fri"}}, // always Mon→Sun order
		{"понедельник, среда и пятница", []string{"Mon", "Wed", "Fri"}},
		{"пн, ср, пт", []string{"Mon", "Wed", "Fri"}},
		{"dushanba, chorshanba, juma", []string{"Mon", "Wed", "Fri"}},
		{"shanba va yakshanba", []string{"Sat", "Sun"}},
		{"tuesday and thursday", []string{"Tue", "Thu"}},
		{"whenever", nil},
	}
	for _, c := range cases {
		if got := detectDays(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("detectDays(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestDetectDaysIsDeterministic(t *testing.T) {
	first := detectDays("mon tue wed thu fri sat sun")
	for i := 0; i < 50; i++ {
		if got := detectDays("mon tue wed thu fri sat sun"); !reflect.DeepEqual(got, first) {
			t.Fatalf("detectDays is not deterministic: %v then %v", first, got)
		}
	}
	want := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("detectDays = %v, want %v", first, want)
	}
}

func TestParseWeekdaySetAcceptsLocalizedNames(t *testing.T) {
	got := parseWeekdaySet([]string{"понедельник", "juma"})
	if !got[time.Monday] || !got[time.Friday] {
		t.Errorf("parseWeekdaySet = %v, want Monday and Friday", got)
	}
	if len(got) != 2 {
		t.Errorf("parseWeekdaySet matched %d days, want 2", len(got))
	}
	fallback := parseWeekdaySet(nil)
	for _, d := range []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday} {
		if !fallback[d] {
			t.Errorf("empty day list should fall back to Mon–Fri, missing %v", d)
		}
	}
	if fallback[time.Saturday] || fallback[time.Sunday] {
		t.Error("Mon–Fri fallback should not include the weekend")
	}
}

// An answer containing a date must not be mistaken for an hours-per-week count.
func TestParseHoursPerWeekIgnoresDates(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"10 hours a week", 10, true},
		{"about 6", 6, true},
		{"2026-12-01", 0, false},
		{"my exam is 2026-12-01, I can do 8 hours", 8, true},
		{"2026", 0, false},
		{"no idea", 0, false},
		{"1000", 0, false},
		{"80", 60, true}, // clamped to the supported maximum
	}
	for _, c := range cases {
		got, ok := parseHoursPerWeek(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseHoursPerWeek(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// A date answer must not become an hours-per-week count — and, since nothing in
// it states an availability, it must not quietly become the six-hour default
// either. Inventing that default is how a learner who never said when they
// could study ended up with a plan sized against time they did not have; the
// interview now comes back and asks instead.
func TestIngestAnswerDoesNotTurnADateIntoHours(t *testing.T) {
	sess := &IntakeSession{Lang: "en", AnswerBag: map[string]string{}}
	for _, msg := range []string{"complete beginner", "band 7.0", "2026-12-01", "2026-12-01"} {
		ingestAnswer(sess, msg)
	}
	if sess.Answers.HoursPerWeek == 60 {
		t.Fatal("a date answer was parsed as 60 hours per week")
	}
	if sess.Answers.HoursPerWeek != 0 || sess.Answers.Availability.HasTime() {
		t.Errorf("HoursPerWeek = %d / availability %+v, want availability left UNKNOWN: a deadline says nothing about study time",
			sess.Answers.HoursPerWeek, sess.Answers.Availability)
	}
	if sess.Answers.Availability.Complete() {
		t.Error("availability must not be complete when the user never gave one")
	}
	if sess.Answers.Deadline != "2026-12-01" {
		t.Errorf("Deadline = %q, want 2026-12-01", sess.Answers.Deadline)
	}
}

// Phase week ranges must stay valid no matter how short the plan is.
func TestMockPhasesAlwaysProduceValidWindows(t *testing.T) {
	for weeks := 1; weeks <= 24; weeks++ {
		phases := mockPhases(weeks, "en")
		if len(phases) == 0 {
			t.Fatalf("weeks=%d produced no phases", weeks)
		}
		prevEnd := 0
		for _, p := range phases {
			if p.WeekStart < 1 || p.WeekEnd > weeks {
				t.Errorf("weeks=%d phase %s window [%d,%d] outside 1..%d", weeks, p.Key, p.WeekStart, p.WeekEnd, weeks)
			}
			if p.WeekEnd < p.WeekStart {
				t.Errorf("weeks=%d phase %s window [%d,%d] is inverted", weeks, p.Key, p.WeekStart, p.WeekEnd)
			}
			if p.WeekStart <= prevEnd {
				t.Errorf("weeks=%d phase %s starts at %d, overlapping previous end %d", weeks, p.Key, p.WeekStart, prevEnd)
			}
			prevEnd = p.WeekEnd
		}
		if last := phases[len(phases)-1]; last.WeekEnd != weeks {
			t.Errorf("weeks=%d last phase ends at %d, want %d", weeks, last.WeekEnd, weeks)
		}
	}
}

func TestMockUnderstandClassification(t *testing.T) {
	cases := []struct {
		msg       string
		wantSkill string
		vague     bool
		inScope   bool
	}{
		{"I want IELTS 7.0", "IELTS", false, true},
		{"learn guitar", "Guitar", false, true},
		{"I want to get fit", "", true, true},
		{"tell me about profit margins", "", false, true}, // "fit" inside "profit" must not trigger fitness
		{"what is the capital of France", "", false, false},
	}
	for _, c := range cases {
		got := mockUnderstand(c.msg, "en")
		if got.InScope != c.inScope {
			t.Errorf("%q: inScope = %v, want %v", c.msg, got.InScope, c.inScope)
		}
		if got.NeedsDisambiguation != c.vague {
			t.Errorf("%q: needsDisambiguation = %v, want %v", c.msg, got.NeedsDisambiguation, c.vague)
		}
		if c.wantSkill != "" && got.Skill != c.wantSkill {
			t.Errorf("%q: skill = %q, want %q", c.msg, got.Skill, c.wantSkill)
		}
	}
}

func TestSanitizeSkillLabelStripsControlCharsAndOverlongText(t *testing.T) {
	if got := sanitizeSkillLabel("a\nb"); got != "a b" {
		t.Errorf("got %q, want %q", got, "a b")
	}
	if got := sanitizeSkillLabel("  spaced   out  "); got != "spaced out" {
		t.Errorf("got %q, want %q", got, "spaced out")
	}
	long := "this goal description is definitely longer than the forty character limit"
	if got := sanitizeSkillLabel(long); got != "" {
		t.Errorf("overlong label should be rejected, got %q", got)
	}
}

// The gap to the deadline must be measured in the user's timezone. Measuring it
// in the server's zone shifts the plan length by a week for users far enough
// east or west of wherever the server happens to run.
func TestMockPlanMeasuresDeadlineInTheUsersTimezone(t *testing.T) {
	for _, zone := range []string{"Asia/Tashkent", "UTC", "Europe/Moscow", "America/New_York", "America/Los_Angeles", "Pacific/Auckland"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatalf("LoadLocation(%s): %v", zone, err)
		}
		for _, days := range []int{14, 28, 56} {
			deadline := dateStr(todayIn(loc).AddDate(0, 0, days))
			sess := &IntakeSession{
				Lang: "en", Skill: "Chess",
				Answers:   FrameworkAnswers{Deadline: deadline, HoursPerWeek: 6},
				AnswerBag: map[string]string{},
			}
			got := mockPlan(sess, loc).WeeksTotal
			want := clamp(days/7+1, 2, 24)
			if got != want {
				t.Errorf("%s: deadline %d days out -> weeksTotal=%d, want %d", zone, days, got, want)
			}
		}
	}
}
