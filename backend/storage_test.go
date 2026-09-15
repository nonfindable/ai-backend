package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// P7: storage safety. The existing suite already covers deep copies, atomic
// snapshots, corrupt-file quarantine and token hashing. These cover the two
// gaps found during this pass.

// An existing snapshot that cannot be READ must never be overwritten. The
// corrupt-JSON path quarantines the file because it can read it; an unreadable
// file cannot be quarantined, so the store must stop writing instead of
// starting empty and destroying the only copy on the next flush.
func TestUnreadableSnapshotIsNotOverwritten(t *testing.T) {
	// A directory where the snapshot file belongs makes ReadFile fail with a
	// non-NotExist error on every OS, which is the condition under test
	// (unreadable but present) without depending on chmod semantics.
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(path, "keepme")
	if err := os.WriteFile(sentinel, []byte("irreplaceable"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newStore(dir)
	if !s.snapshotsOff {
		t.Error("an unreadable snapshot did not disable writing")
	}
	s.SaveUser(&User{ID: "user_new", Name: "Should Not Persist"})
	s.Flush()
	s.Close()

	// Nothing at that path was replaced or removed.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the existing path was destroyed: %v", err)
	}
	if !fi.IsDir() {
		t.Fatal("the existing path was overwritten by a snapshot")
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "irreplaceable" {
		t.Errorf("pre-existing data was lost: %q, %v", got, err)
	}
}

// The readable-but-corrupt path must still quarantine rather than destroy.
func TestCorruptSnapshotStillQuarantinesAndKeepsWriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	if err := os.WriteFile(path, []byte("This is not JSON"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newStore(dir)
	s.SaveUser(&User{ID: "user_new", Name: "Fresh Start"})
	s.Flush()
	s.Close()

	// The bad file was preserved under a .corrupt.* name...
	matches, _ := filepath.Glob(filepath.Join(dir, "store.json.corrupt.*"))
	if len(matches) == 0 {
		t.Error("the corrupt snapshot was not quarantined")
	}
	// ...and the store resumed writing a valid one.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no new snapshot written: %v", err)
	}
	var shape persistShape
	if err := json.Unmarshal(data, &shape); err != nil {
		t.Fatalf("new snapshot is not valid JSON: %v", err)
	}
	if _, ok := shape.Users["user_new"]; !ok {
		t.Error("the new snapshot lost the write made after quarantine")
	}
}

// Availability updates are a read-modify-write on a shared record. Done through
// GetUser/SaveUser they lost writes under concurrency; UpdateUserAvailability
// performs both halves under the store's own lock.
func TestConcurrentAvailabilityUpdatesDoNotLoseWrites(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	t.Cleanup(s.Close)
	s.SaveUser(&User{ID: "u1", Name: "Learner"})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); s.UpdateUserAvailability("u1", 10, []string{"Mon", "Wed"}) }()
		go func() { defer wg.Done(); s.SaveToken(newToken(), "u1") }()
	}
	wg.Wait()

	u, ok := s.GetUser("u1")
	if !ok {
		t.Fatal("user vanished")
	}
	if u.HoursPerWeek != 10 {
		t.Errorf("hoursPerWeek = %d, want 10", u.HoursPerWeek)
	}
	if len(u.Days) != 2 || u.Days[0] != "Mon" {
		t.Errorf("days = %v, want [Mon Wed]", u.Days)
	}
}

// A no-op update must not falsely report a change, and must leave the record be.
func TestAvailabilityUpdateIgnoresEmptyInput(t *testing.T) {
	s := newStore(t.TempDir())
	t.Cleanup(s.Close)
	s.SaveUser(&User{ID: "u1", HoursPerWeek: 8, Days: []string{"Tue"}})

	s.UpdateUserAvailability("u1", 0, nil)
	u, _ := s.GetUser("u1")
	if u.HoursPerWeek != 8 || len(u.Days) != 1 || u.Days[0] != "Tue" {
		t.Errorf("an empty update mutated the record: %+v", u)
	}
	s.UpdateUserAvailability("does_not_exist", 5, []string{"Mon"})
}

// The returned slice must be a copy: a caller mutating it must not reach into
// the store.
func TestAvailabilityUpdateCopiesTheDaysSlice(t *testing.T) {
	s := newStore(t.TempDir())
	t.Cleanup(s.Close)
	s.SaveUser(&User{ID: "u1"})

	days := []string{"Mon", "Wed"}
	s.UpdateUserAvailability("u1", 6, days)
	days[0] = "MUTATED"

	u, _ := s.GetUser("u1")
	if u.Days[0] != "Mon" {
		t.Errorf("the store aliased the caller's slice: %v", u.Days)
	}
}
