package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A change agreed at the confirmation gate must actually reach the answers the
// plan is built from. Days were missing from the decision, so a user who moved
// their study days there saw the new days echoed back in the recap, approved
// it, and got a calendar built on the days from the interview instead.
func TestConfirmDecisionAppliesEveryChangedField(t *testing.T) {
	sess := &IntakeSession{
		Answers: FrameworkAnswers{
			Target: "B1", Deadline: "2027-01-31", HoursPerWeek: 6,
			Days: []string{"Tue", "Wed", "Sat"},
		},
	}
	applyFeasibilityDecision(sess, feasibilityDecision{
		Resolved: true, Target: "advanced", Deadline: "2027-05-31", HoursPerWeek: 14,
		Days: []string{"Monday", "Tuesday", "Friday"},
	}, "make it 14 hours a week on Monday, Tuesday and Friday")

	a := sess.Answers
	if a.Target != "advanced" || a.Deadline != "2027-05-31" || a.HoursPerWeek != 14 {
		t.Errorf("target/deadline/hours = %q/%q/%d, want advanced/2027-05-31/14", a.Target, a.Deadline, a.HoursPerWeek)
	}
	if got := strings.Join(a.Days, ","); got != "Mon,Tue,Fri" {
		t.Errorf("days = %q, want the canonical codes Mon,Tue,Fri", got)
	}
}

// Values the user did not touch must survive, and a day list that cannot be
// parsed must leave the existing one alone rather than emptying the week.
func TestConfirmDecisionLeavesUntouchedFieldsAlone(t *testing.T) {
	sess := &IntakeSession{
		Answers: FrameworkAnswers{
			Target: "B1", Deadline: "2027-01-31", HoursPerWeek: 6,
			Days: []string{"Mon", "Wed", "Fri"},
		},
	}
	applyFeasibilityDecision(sess, feasibilityDecision{Resolved: true, HoursPerWeek: 10}, "make it 10 hours a week")
	a := sess.Answers
	if a.Target != "B1" || a.Deadline != "2027-01-31" {
		t.Errorf("an hours-only change disturbed the goal: %q / %q", a.Target, a.Deadline)
	}
	if a.HoursPerWeek != 10 {
		t.Errorf("hoursPerWeek = %d, want 10", a.HoursPerWeek)
	}
	if strings.Join(a.Days, ",") != "Mon,Wed,Fri" {
		t.Errorf("days = %v, want them untouched", a.Days)
	}

	applyFeasibilityDecision(sess, feasibilityDecision{Resolved: true, Days: []string{"someday", "whenever"}}, "someday and whenever, 10 hours a week")
	if strings.Join(sess.Answers.Days, ",") != "Mon,Wed,Fri" {
		t.Errorf("days = %v, want the previous set kept when nothing parses", sess.Answers.Days)
	}
}

// An out-of-range week is clamped rather than handed to the scheduler.
func TestConfirmDecisionClampsHours(t *testing.T) {
	sess := &IntakeSession{Answers: FrameworkAnswers{HoursPerWeek: 6}}
	applyFeasibilityDecision(sess, feasibilityDecision{Resolved: true, HoursPerWeek: 60}, "make it 60 hours a week")
	if sess.Answers.HoursPerWeek != 40 {
		t.Errorf("hoursPerWeek = %d, want it clamped to 40", sess.Answers.HoursPerWeek)
	}
}

// The mock gate must only treat an unambiguous go-ahead as approval. "not
// sure" matched the "sure" stem and built a plan for someone who had just said
// they were undecided.
func TestMockApprovalNeedsAnUnambiguousYes(t *testing.T) {
	approve := []string{"yes", "Yes, build my plan", "ok go ahead", "да, стройте", "ha, tuzing"}
	refuse := []string{"hmm not sure", "no", "not yet", "wait", "change something",
		"I'd rather do 10 hours", "нет", "подождите", "yo'q"}

	for _, s := range approve {
		if !approvesPlan(s) {
			t.Errorf("%q should be read as approval", s)
		}
	}
	for _, s := range refuse {
		if approvesPlan(s) {
			t.Errorf("%q must NOT build a plan", s)
		}
	}
}

// Models disagree about whether a number is a number or a string, and some
// emit both shapes across runs. Every consumer downstream works on strings, so
// the decoder coerces rather than trusting the model to quote its own output:
// an unquoted hoursPerWeek used to fail the whole intake stage with
// "cannot unmarshal number into Go struct field ... of type string".
func TestIntakeAnswersAcceptEitherJSONShape(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{
			"numbers and arrays unquoted",
			`{"answers":{"hoursPerWeek":14,"days":["Mon","Tue","Fri"],"currentLevel":"A1"}}`,
			map[string]string{"hoursPerWeek": "14", "days": "Mon,Tue,Fri", "currentLevel": "A1"},
		},
		{
			"everything quoted",
			`{"answers":{"hoursPerWeek":"14","days":"Mon,Tue,Fri","currentLevel":"A1"}}`,
			map[string]string{"hoursPerWeek": "14", "days": "Mon,Tue,Fri", "currentLevel": "A1"},
		},
		{
			"nulls, objects and floats collapse safely",
			`{"answers":{"hoursPerWeek":7.5,"deadline":null,"budget":{"per":"month"},"motivation":true}}`,
			map[string]string{"hoursPerWeek": "7.5", "deadline": "", "budget": "", "motivation": "true"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r intakeResult
			if err := json.Unmarshal([]byte(tc.raw), &r); err != nil {
				t.Fatalf("decode failed: %v", err)
			}
			for k, want := range tc.want {
				if got := r.Answers[k]; got != want {
					t.Errorf("answers[%q] = %q, want %q", k, got, want)
				}
			}
		})
	}
}

// And the coerced values must survive the trip into the session.
func TestNumericHoursReachTheAnswers(t *testing.T) {
	var r intakeResult
	if err := json.Unmarshal([]byte(`{"answers":{"hoursPerWeek":14,"days":["Mon","Tue","Fri"]}}`), &r); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	// applyAnswers no longer writes availability: the model's figures are a
	// claim that has to be corroborated by the learner's own words first. What
	// is pinned here is that the coerced values decode into the right shape.
	av := availabilityFromModel(r.Answers)
	if av.HoursPerWeek() != 14 {
		t.Errorf("hoursPerWeek = %d, want 14", av.HoursPerWeek())
	}
	if strings.Join(av.Days, ",") != "Mon,Tue,Fri" {
		t.Errorf("days = %v, want Mon,Tue,Fri", av.Days)
	}

	// And that they do reach the session when the learner really said them.
	sess := &IntakeSession{}
	recordStatedAvailability(sess, "14 hours a week on Mon, Tue and Fri", "timeBudget", r.Answers)
	if sess.Answers.HoursPerWeek != 14 {
		t.Errorf("hoursPerWeek = %d, want 14", sess.Answers.HoursPerWeek)
	}
	if strings.Join(sess.Answers.Days, ",") != "Mon,Tue,Fri" {
		t.Errorf("days = %v, want Mon,Tue,Fri", sess.Answers.Days)
	}
}
