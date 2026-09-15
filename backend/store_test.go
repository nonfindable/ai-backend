package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := newStore(t.TempDir())
	t.Cleanup(s.Close)
	return s
}

// Readers must not receive pointers into the store's maps, or a handler
// mutating what it read will race the snapshot goroutine serializing it.
func TestStoreReturnsIsolatedCopies(t *testing.T) {
	s := newTestStore(t)

	sess := &IntakeSession{ID: "s1", UserID: "u1", AnswerBag: map[string]string{"k": "v"},
		Messages: []Message{{Role: "user", Content: "hi"}}}
	s.SaveSession(sess)

	// Mutating what we handed in must not change what is stored.
	sess.AnswerBag["k"] = "mutated"
	sess.Messages[0].Content = "mutated"

	got, ok := s.GetSession("s1")
	if !ok {
		t.Fatal("session missing")
	}
	if got.AnswerBag["k"] != "v" || got.Messages[0].Content != "hi" {
		t.Error("SaveSession stored a live reference to the caller's object")
	}

	// Mutating what we read must not change what is stored either.
	got.AnswerBag["k"] = "changed"
	got.Messages[0].Content = "changed"
	again, _ := s.GetSession("s1")
	if again.AnswerBag["k"] != "v" || again.Messages[0].Content != "hi" {
		t.Error("GetSession returned a live reference into the store")
	}
}

func TestStorePlanCopiesAreDeep(t *testing.T) {
	s := newTestStore(t)
	p := &Plan{ID: "p1", UserID: "u1", Days: []string{"Mon"},
		Todos: []Todo{{ID: "t1", Title: "orig", DependsOn: []string{"x"}}}}
	s.SavePlan(p)

	got, _ := s.GetPlan("p1")
	got.Todos[0].Title = "changed"
	got.Todos[0].DependsOn[0] = "changed"
	got.Days[0] = "Sun"

	again, _ := s.GetPlan("p1")
	if again.Todos[0].Title != "orig" || again.Todos[0].DependsOn[0] != "x" || again.Days[0] != "Mon" {
		t.Error("plan clone is shallow; nested slices are shared with the store")
	}
}

// Concurrent traffic across many entities must not lose writes or trip Go's
// concurrent-map detector.
func TestStoreConcurrentWritesAreSafe(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				id := "s" + itoa(i)
				s.SaveSession(&IntakeSession{
					ID: id, UserID: "u" + itoa(i),
					AnswerBag: map[string]string{"n": itoa(j)},
					Messages:  []Message{{Role: "user", Content: itoa(j)}},
				})
				if got, ok := s.GetSession(id); ok {
					_ = got.AnswerBag["n"]
				}
				s.EventsForUser("u" + itoa(i))
				s.PlansByUser("u" + itoa(i))
			}
		}(i)
	}
	wg.Wait()
	if len(s.UserIDs()) != 0 {
		t.Log("no users were created in this test, as expected")
	}
	for i := 0; i < 40; i++ {
		if _, ok := s.GetSession("s" + itoa(i)); !ok {
			t.Errorf("session s%d went missing under concurrency", i)
		}
	}
}

func TestStoreSnapshotIsAtomicAndReloadable(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	s.SaveUser(&User{ID: "u1", Name: "Alice", Timezone: "UTC", CreatedAt: time.Now()})
	s.SavePlan(&Plan{ID: "p1", UserID: "u1", Skill: "IELTS"})
	s.Close() // flushes

	data, err := os.ReadFile(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	var shape persistShape
	if err := json.Unmarshal(data, &shape); err != nil {
		t.Fatalf("snapshot is not valid JSON: %v", err)
	}

	reopened := newStore(dir)
	defer reopened.Close()
	if u, ok := reopened.GetUser("u1"); !ok || u.Name != "Alice" {
		t.Error("user did not survive a restart")
	}
	if p, ok := reopened.GetPlan("p1"); !ok || p.Skill != "IELTS" {
		t.Error("plan did not survive a restart")
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("left a temp file behind: %s", e.Name())
		}
	}
}

// A corrupt snapshot must be preserved, not silently replaced by an empty one.
func TestStoreCorruptSnapshotIsPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	if err := os.WriteFile(path, []byte(`{"users": {"u1": TRUNCATED`), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newStore(dir)
	s.SaveUser(&User{ID: "u2", Name: "Bob"})
	s.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt.") {
			found = true
			body, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			if !strings.Contains(string(body), "TRUNCATED") {
				t.Error("the preserved file does not contain the original bytes")
			}
		}
	}
	if !found {
		t.Error("a corrupt store was discarded instead of being moved aside for recovery")
	}
}

func TestStoreTokensAreHashedNotStoredInClear(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	token := newToken()
	s.SaveUser(&User{ID: "u1"})
	s.SaveToken(token, "u1")
	s.Close()

	data, err := os.ReadFile(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Error("the bearer token is stored in clear text in the snapshot")
	}

	reopened := newStore(dir)
	defer reopened.Close()
	if u, ok := reopened.UserByToken(token); !ok || u.ID != "u1" {
		t.Error("token lookup broke across a restart")
	}
	if _, ok := reopened.UserByToken("not-a-real-token"); ok {
		t.Error("an unknown token resolved to a user")
	}
}

func TestEventsAreSortedDeterministically(t *testing.T) {
	s := newTestStore(t)
	s.SaveEvents([]*CalendarEvent{
		{ID: "e3", PlanID: "p", UserID: "u", Date: "2026-01-02", StartTime: "18:00"},
		{ID: "e1", PlanID: "p", UserID: "u", Date: "2026-01-01", StartTime: "19:00"},
		{ID: "e2", PlanID: "p", UserID: "u", Date: "2026-01-01", StartTime: "18:00"},
	})
	want := []string{"e2", "e1", "e3"}
	for i := 0; i < 20; i++ {
		got := s.EventsForPlan("p")
		for j, id := range want {
			if got[j].ID != id {
				t.Fatalf("position %d = %s, want %s", j, got[j].ID, id)
			}
		}
	}
}
