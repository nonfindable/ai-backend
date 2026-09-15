package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is an in-memory data store with a JSON-file snapshot, so an MVP demo
// survives restarts with zero external dependencies. Swap for Postgres in prod.
//
// Two invariants keep it safe under concurrent HTTP traffic:
//
//  1. Nothing outside this file ever holds a pointer into the maps. Readers get
//     deep clones and writers hand over clones, so a handler mutating a plan can
//     never race the snapshot goroutine serializing it.
//  2. Snapshots are written by a single background goroutine, debounced, to a
//     temp file that is then renamed over the real one. Writing the whole store
//     synchronously inside every request made a single insert take seconds once
//     the file reached a few megabytes, and a crash mid-write truncated
//     everything.
//
// # WHAT THIS IS NOT SAFE FOR
//
// This store is SINGLE-INSTANCE ONLY. It is correct under concurrent goroutines
// inside one process and nothing more. Running two replicas against the same
// DATA_DIR corrupts data; running them against different directories silently
// splits the user base in two. Specifically:
//
//   - No cross-process locking. Two processes sharing a DATA_DIR both hold the
//     whole dataset in memory and both rename their own full snapshot over the
//     file, so the last writer wins and the other's writes are simply gone.
//   - The per-plan mutation locks (keyedMutex, see Scheduler.planLocks) are
//     in-process mutexes. They order writers inside ONE binary and provide no
//     mutual exclusion between replicas.
//   - Bearer tokens live in this map, so a second instance cannot authenticate
//     a user created by the first.
//   - The whole dataset is held in memory and rewritten in full on every flush;
//     cost is O(total data) per write, which bounds it to demo-scale volumes.
//   - DATA_DIR must be durable. On an ephemeral filesystem (a container without
//     a mounted disk) every restart loses all users, tokens, plans and the AI
//     gateway's spend counters, which silently resets the monthly cost cap.
//
// Horizontal scaling requires replacing this layer (Postgres, and a shared lock
// or transactional writes for the plan mutation boundary). Until then, deploy
// exactly one instance with a persistent volume.
type Store struct {
	mu       sync.RWMutex
	dir      string
	Users    map[string]*User
	Tokens   map[string]string // sha256(bearer token) -> userID
	Sessions map[string]*IntakeSession
	Goals    map[string]*Goal
	Plans    map[string]*Plan
	Events   map[string]*CalendarEvent
	Progress []*ProgressLog

	dirty    chan struct{}
	done     chan struct{}
	closeOne sync.Once
	wg       sync.WaitGroup
	debounce time.Duration

	// snapshotsOff is set when the on-disk snapshot exists but could not be
	// read. It is written once during load(), before the snapshot goroutine
	// starts, and only read afterwards.
	snapshotsOff bool
}

type persistShape struct {
	Users    map[string]*User          `json:"users"`
	Tokens   map[string]string         `json:"tokens"`
	Sessions map[string]*IntakeSession `json:"sessions"`
	Goals    map[string]*Goal          `json:"goals"`
	Plans    map[string]*Plan          `json:"plans"`
	Events   map[string]*CalendarEvent `json:"events"`
	Progress []*ProgressLog            `json:"progress"`
}

func newStore(dir string) *Store {
	s := &Store{
		dir:      dir,
		Users:    map[string]*User{},
		Tokens:   map[string]string{},
		Sessions: map[string]*IntakeSession{},
		Goals:    map[string]*Goal{},
		Plans:    map[string]*Plan{},
		Events:   map[string]*CalendarEvent{},
		dirty:    make(chan struct{}, 1),
		done:     make(chan struct{}),
		debounce: 250 * time.Millisecond,
	}
	s.load()
	s.wg.Add(1)
	go s.snapshotLoop()
	return s
}

func (s *Store) path() string { return filepath.Join(s.dir, "store.json") }

// hashToken stores only a digest, so a leaked snapshot file does not hand over
// working credentials.
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func (s *Store) load() {
	data, err := os.ReadFile(s.path())
	if err != nil {
		if os.IsNotExist(err) {
			return // first run
		}
		// The file is there but unreadable (permissions, a bad disk). Starting
		// empty and letting the next snapshot overwrite it would destroy the
		// only copy, so refuse to write at all until an operator intervenes.
		// The corrupt-JSON path below can quarantine the file because it can
		// read it; this path cannot, so it must not touch it.
		log.Printf("store: cannot read %s: %v — snapshots DISABLED to protect the existing file; "+
			"state changes will be lost on restart until this is resolved", s.path(), err)
		s.snapshotsOff = true
		return
	}
	var p persistShape
	if err := json.Unmarshal(data, &p); err != nil {
		// Do not start empty and then overwrite the only copy of the data on the
		// next write — move it aside so it can be inspected or recovered.
		backup := s.path() + fmt.Sprintf(".corrupt.%d", time.Now().Unix())
		if rerr := os.Rename(s.path(), backup); rerr != nil {
			log.Printf("store: %s is corrupt (%v) and could not be moved aside: %v", s.path(), err, rerr)
		} else {
			log.Printf("store: %s is corrupt (%v); preserved as %s, starting empty", s.path(), err, backup)
		}
		return
	}
	if p.Users != nil {
		s.Users = p.Users
	}
	if p.Tokens != nil {
		s.Tokens = p.Tokens
	}
	if p.Sessions != nil {
		s.Sessions = p.Sessions
	}
	if p.Goals != nil {
		s.Goals = p.Goals
	}
	if p.Plans != nil {
		s.Plans = p.Plans
	}
	if p.Events != nil {
		s.Events = p.Events
	}
	s.Progress = p.Progress
}

// markDirty asks the snapshot goroutine for a write. Never blocks.
func (s *Store) markDirty() {
	select {
	case s.dirty <- struct{}{}:
	default: // a write is already pending; it will pick up this change too
	}
}

func (s *Store) snapshotLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			s.writeSnapshot()
			return
		case <-s.dirty:
			// Coalesce a burst of writes into one snapshot.
			t := time.NewTimer(s.debounce)
			select {
			case <-t.C:
			case <-s.done:
				t.Stop()
				s.writeSnapshot()
				return
			}
			s.writeSnapshot()
		}
	}
}

func (s *Store) writeSnapshot() {
	if s.snapshotsOff {
		return
	}
	s.mu.RLock()
	p := persistShape{s.Users, s.Tokens, s.Sessions, s.Goals, s.Plans, s.Events, s.Progress}
	data, err := json.MarshalIndent(p, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		log.Printf("store: marshal failed: %v", err)
		return
	}
	if err := atomicWriteFile(s.path(), data, 0o600); err != nil {
		log.Printf("store: snapshot failed: %v", err)
	}
}

// atomicWriteFile writes to a sibling temp file and renames it into place, so a
// crash or a full disk can never leave a half-written store behind.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Flush blocks until pending changes are on disk. Used at shutdown and in tests.
func (s *Store) Flush() { s.writeSnapshot() }

// Close stops the snapshot goroutine after one final write.
func (s *Store) Close() {
	s.closeOne.Do(func() { close(s.done) })
	s.wg.Wait()
}

// ---- Users ----

func (s *Store) SaveUser(u *User) {
	s.mu.Lock()
	s.Users[u.ID] = u.clone()
	s.mu.Unlock()
	s.markDirty()
}

func (s *Store) GetUser(id string) (*User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.Users[id]
	return u.clone(), ok
}

// SaveToken binds a bearer token to a user. Only the digest is stored.
func (s *Store) SaveToken(token, userID string) {
	s.mu.Lock()
	s.Tokens[hashToken(token)] = userID
	s.mu.Unlock()
	s.markDirty()
}

// UserByToken resolves a bearer token to its user.
func (s *Store) UserByToken(token string) (*User, bool) {
	if token == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.Tokens[hashToken(token)]
	if !ok {
		return nil, false
	}
	u, ok := s.Users[id]
	return u.clone(), ok
}

// UpdateUserAvailability merges the availability learned while building a plan
// into the profile, atomically. Callers used to do GetUser -> mutate the clone
// -> SaveUser, which is a read-modify-write over a shared record with no lock
// held between the halves: a concurrent session update or a second plan could
// silently drop one of the two writes.
func (s *Store) UpdateUserAvailability(userID string, hoursPerWeek int, days []string) {
	s.mu.Lock()
	u, ok := s.Users[userID]
	if !ok {
		s.mu.Unlock()
		return
	}
	changed := false
	if hoursPerWeek > 0 && u.HoursPerWeek != hoursPerWeek {
		u.HoursPerWeek = hoursPerWeek
		changed = true
	}
	if len(days) > 0 && strings.Join(u.Days, ",") != strings.Join(days, ",") {
		u.Days = append([]string(nil), days...)
		changed = true
	}
	s.mu.Unlock()
	if changed {
		s.markDirty()
	}
}

func (s *Store) UserIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.Users))
	for id := range s.Users {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ---- Sessions ----

func (s *Store) SaveSession(sess *IntakeSession) {
	c := sess.clone()
	c.UpdatedAt = time.Now()
	s.mu.Lock()
	s.Sessions[c.ID] = c
	s.mu.Unlock()
	s.markDirty()
}

func (s *Store) GetSession(id string) (*IntakeSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.Sessions[id]
	return sess.clone(), ok
}

// ---- Goals ----

func (s *Store) SaveGoal(g *Goal) {
	s.mu.Lock()
	s.Goals[g.ID] = g.clone()
	s.mu.Unlock()
	s.markDirty()
}

// ---- Plans ----

func (s *Store) SavePlan(p *Plan) {
	c := p.clone()
	c.UpdatedAt = time.Now()
	s.mu.Lock()
	s.Plans[c.ID] = c
	s.mu.Unlock()
	s.markDirty()
}

func (s *Store) GetPlan(id string) (*Plan, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.Plans[id]
	return p.clone(), ok
}

func (s *Store) PlansByUser(userID string) []*Plan {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*Plan{}
	for _, p := range s.Plans {
		if p.UserID == userID {
			out = append(out, p.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// ---- Events ----

func (s *Store) SaveEvents(evs []*CalendarEvent) {
	s.mu.Lock()
	for _, ev := range evs {
		s.Events[ev.ID] = ev.clone()
	}
	s.mu.Unlock()
	s.markDirty()
}

func (s *Store) ReplaceEventsForPlan(planID string, evs []*CalendarEvent) {
	s.mu.Lock()
	for id, ev := range s.Events {
		if ev.PlanID == planID {
			delete(s.Events, id)
		}
	}
	for _, ev := range evs {
		s.Events[ev.ID] = ev.clone()
	}
	s.mu.Unlock()
	s.markDirty()
}

func (s *Store) EventsForPlan(planID string) []*CalendarEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*CalendarEvent{} // non-nil so JSON is [] not null
	for _, ev := range s.Events {
		if ev.PlanID == planID {
			out = append(out, ev.clone())
		}
	}
	sortEvents(out)
	return out
}

func (s *Store) EventsForUser(userID string) []*CalendarEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*CalendarEvent{} // non-nil so JSON is [] not null
	for _, ev := range s.Events {
		if ev.UserID == userID {
			out = append(out, ev.clone())
		}
	}
	sortEvents(out)
	return out
}

// sortEvents orders by date, then time, then ID so the order is total and
// stable rather than dependent on map iteration.
func sortEvents(evs []*CalendarEvent) {
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].Date != evs[j].Date {
			return evs[i].Date < evs[j].Date
		}
		if evs[i].StartTime != evs[j].StartTime {
			return evs[i].StartTime < evs[j].StartTime
		}
		return evs[i].ID < evs[j].ID
	})
}

// ---- Progress ----

func (s *Store) AddProgress(p *ProgressLog) {
	s.mu.Lock()
	s.Progress = append(s.Progress, p.clone())
	s.mu.Unlock()
	s.markDirty()
}

func (s *Store) ProgressForUser(userID string) []*ProgressLog {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*ProgressLog{}
	for _, p := range s.Progress {
		if p.UserID == userID {
			out = append(out, p.clone())
		}
	}
	return out
}
