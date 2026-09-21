package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// P4: /api/chat must expose enough structure that a frontend switches on
// fields, never on prose. These tests pin the contract.

func chatTurn(t *testing.T, c *client, sessionID, msg string) Turn {
	t.Helper()
	resp := c.request("POST", "/api/chat", map[string]any{"sessionId": sessionID, "message": msg})
	defer resp.Body.Close()
	var turn Turn
	if err := json.NewDecoder(resp.Body).Decode(&turn); err != nil {
		t.Fatalf("decode turn: %v", err)
	}
	return turn
}

// The documented stages, and nothing else, may appear.
func TestChatOnlyEmitsDocumentedStages(t *testing.T) {
	valid := map[string]bool{
		"scope_check": true, "out_of_scope": true, "disambiguation": true,
		"intake": true, "confirm_plan": true, "plan_ready": true,
	}
	ts := newTestServer(t)
	c := ts.newClient(t)
	sess := newSession(t, c)

	// The last message is the go-ahead: nothing is built until the user
	// approves the recap at the confirm_plan stage.
	script := []string{"I want IELTS 7.0", "Academic", "band 5.5", "band 7.0",
		"2026-12-01", "10 hours a week on Mon Wed Fri", "free materials",
		"yes, build my plan"}
	for _, msg := range script {
		turn := chatTurn(t, c, sess, msg)
		if !valid[turn.Stage] {
			t.Fatalf("undocumented stage %q", turn.Stage)
		}
		if turn.SessionID != sess {
			t.Errorf("turn did not echo the session id: %q", turn.SessionID)
		}
	}
}

// An out-of-scope message must be a distinct, machine-readable state.
func TestOutOfScopeIsAnExplicitStage(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	sess := newSession(t, c)

	turn := chatTurn(t, c, sess, "what is the capital of France")
	if turn.Stage != "out_of_scope" {
		t.Fatalf("stage = %q, want out_of_scope", turn.Stage)
	}
	if turn.Assistant == "" {
		t.Error("out_of_scope carried no redirect text")
	}
	if turn.PlanID != "" || turn.Done {
		t.Error("out_of_scope must not look finished")
	}
}

// Disambiguation must carry its question and quick replies explicitly.
func TestDisambiguationExposesQuestionAndOptions(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	sess := newSession(t, c)

	turn := chatTurn(t, c, sess, "I want to be a gamer")
	if turn.Stage != "disambiguation" {
		t.Fatalf("stage = %q, want disambiguation", turn.Stage)
	}
	if turn.Question == "" {
		t.Error("disambiguation did not expose the question separately from prose")
	}
	if len(turn.Options) < 2 {
		t.Errorf("disambiguation offered %d options, want at least 2", len(turn.Options))
	}
}

// Intake must report progress, and must be honest that the interview is
// adaptive rather than a fixed-length form.
func TestIntakeReportsAdaptiveProgress(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	sess := newSession(t, c)

	turn := chatTurn(t, c, sess, "I want to learn chess")
	if turn.Stage != "intake" {
		t.Fatalf("stage = %q, want intake", turn.Stage)
	}
	if turn.Progress == nil {
		t.Fatal("intake carried no progress block")
	}
	if turn.Progress.Max != maxIntakeQuestions {
		t.Errorf("progress.max = %d, want %d", turn.Progress.Max, maxIntakeQuestions)
	}
	if !turn.Progress.Adaptive {
		t.Error("progress.adaptive must be true: max is a ceiling, not a total")
	}
	if turn.Question == "" {
		t.Error("intake did not expose the bare question")
	}
	if !strings.Contains(turn.Assistant, turn.Question) {
		t.Error("assistant prose should contain the question it is wrapping")
	}

	prev := turn.Progress.Answered
	for _, msg := range []string{"beginner", "club level", "no deadline"} {
		turn = chatTurn(t, c, sess, msg)
		if turn.Stage != "intake" {
			break
		}
		if turn.Progress == nil {
			t.Fatal("progress disappeared mid-interview")
		}
		if turn.Progress.Answered <= prev {
			t.Errorf("progress.answered did not advance: %d -> %d", prev, turn.Progress.Answered)
		}
		if turn.Progress.Answered > turn.Progress.Max {
			t.Errorf("progress.answered %d exceeded the declared ceiling %d",
				turn.Progress.Answered, turn.Progress.Max)
		}
		prev = turn.Progress.Answered
	}
}

// The interview must terminate within the ceiling it advertises.
func TestIntakeTerminatesWithinItsDeclaredCeiling(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	sess := newSession(t, c)

	turn := chatTurn(t, c, sess, "I want to learn drawing")
	asked := 0
	for turn.Stage == "intake" && asked <= maxIntakeQuestions+2 {
		turn = chatTurn(t, c, sess, "something")
		asked++
	}
	// The interview ends at the recap, which has to be approved before a plan
	// exists. That approval is not one of the interview's questions.
	if turn.Stage == "confirm_plan" {
		turn = chatTurn(t, c, sess, "yes, build my plan")
	}
	if turn.Stage != "plan_ready" {
		t.Fatalf("interview did not finish: stage %q after %d answers", turn.Stage, asked)
	}
	if asked > maxIntakeQuestions {
		t.Errorf("asked %d questions, ceiling is %d", asked, maxIntakeQuestions)
	}
}

// plan_ready must be self-describing: a plan id and a done flag.
func TestPlanReadyCarriesPlanIDAndDone(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	sessID, planID := c.buildPlan()

	if planID == "" {
		t.Fatal("no plan id")
	}
	// A further message in plan_ready stays in plan_ready and keeps pointing at
	// the plan, so a reloading client can recover its state from one turn.
	turn := chatTurn(t, c, sessID, "thanks")
	if turn.Stage != "plan_ready" {
		t.Errorf("stage = %q, want plan_ready", turn.Stage)
	}
	if turn.PlanID != planID {
		t.Errorf("planId = %q, want %q", turn.PlanID, planID)
	}
}

// Progress is meaningless outside the interview and must be omitted, not zeroed,
// so a client cannot mistake "0 of 6" for a real position.
func TestProgressIsAbsentOutsideIntake(t *testing.T) {
	ts := newTestServer(t)
	c := ts.newClient(t)
	sess := newSession(t, c)

	turn := chatTurn(t, c, sess, "what is the weather today")
	if turn.Progress != nil {
		t.Errorf("out_of_scope carried a progress block: %+v", turn.Progress)
	}

	resp := c.request("POST", "/api/chat", map[string]any{"sessionId": sess, "message": "tell me a joke"})
	defer resp.Body.Close()
	var raw map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	if _, present := raw["progress"]; present {
		t.Error("progress key should be omitted entirely outside intake")
	}
}
