package main

import (
	"io"
	"strings"
	"testing"
	"time"
)

// P5: the plan's own timezone owns every date. Nothing may fall through to the
// server machine's zone, and the same plan must mean the same instants
// regardless of which device or host looked at it.

// Two zones far enough apart that a same-day/different-instant bug is obvious.
const (
	zoneEast = "Pacific/Auckland"    // UTC+12/+13
	zoneWest = "America/Los_Angeles" // UTC-8/-7
)

func planInZone(zone string) *Plan {
	return &Plan{
		ID: "plan_tz", UserID: "u1", Skill: "Guitar", Timezone: zone, Lang: "en",
		HoursPerWeek: 6, Days: []string{"Mon", "Wed", "Fri"}, WeeksTotal: 4,
		StartDate: "2026-10-05",
		Todos: []Todo{{
			ID: "todo_1", PlanID: "plan_tz", Title: "Practice chords",
			DurationMin: 60, Frequency: "weekly", Priority: "high", Status: "pending",
		}},
	}
}

// An unknown zone must not silently become the server's zone.
func TestUnknownTimezoneFallsBackToUTCNotServerLocal(t *testing.T) {
	loc := loadLocation("Mars/Olympus_Mons")
	if loc != time.UTC {
		t.Errorf("unknown zone resolved to %v, want UTC", loc)
	}
	if loadLocation("") != time.UTC {
		t.Error("empty zone must resolve to UTC, not the host's zone")
	}
	if got := loadLocation("Asia/Tashkent"); got.String() != "Asia/Tashkent" {
		t.Errorf("valid zone resolved to %v", got)
	}
}

// resolveTimezone is what stops an unresolvable name from ever being stored.
func TestResolveTimezoneKeepsOnlyRealZones(t *testing.T) {
	cases := []struct{ in, fallback, want string }{
		{"Asia/Tashkent", "UTC", "Asia/Tashkent"},
		{"Not/AZone", "Asia/Tashkent", "Asia/Tashkent"},
		{"", "Europe/Moscow", "Europe/Moscow"},
		{"Not/AZone", "Also/Bogus", "UTC"},
		{"  Pacific/Auckland  ", "UTC", "Pacific/Auckland"},
	}
	for _, tc := range cases {
		if got := resolveTimezone(tc.in, tc.fallback); got != tc.want {
			t.Errorf("resolveTimezone(%q, %q) = %q, want %q", tc.in, tc.fallback, got, tc.want)
		}
	}
}

// A client that sends a bogus zone gets the configured default stored, not the
// bogus string and not the host's zone.
func TestSessionStoresOnlyAResolvableTimezone(t *testing.T) {
	ts := newTestServerCfg(t, func(c *Config) { c.DefaultTimezone = "Asia/Tashkent" })
	c := &client{t: t, base: ts.URL}
	var out map[string]any
	c.do("POST", "/api/session", map[string]any{"lang": "en", "timezone": "Middle/Earth"}, &out)
	c.token, _ = out["token"].(string)
	c.user, _ = out["userId"].(string)

	u, ok := ts.store.GetUser(c.user)
	if !ok {
		t.Fatal("user not stored")
	}
	if u.Timezone != "Asia/Tashkent" {
		t.Errorf("stored timezone %q, want the configured default", u.Timezone)
	}
	if _, err := time.LoadLocation(u.Timezone); err != nil {
		t.Errorf("stored timezone does not resolve: %v", err)
	}
}

// The same local wall-clock session in two zones must become two different UTC
// instants in the .ics, separated by the zones' offset difference.
func TestICSResolvesLocalTimesAgainstThePlanTimezone(t *testing.T) {
	sc := newScheduler(newStore(t.TempDir()))
	t.Cleanup(sc.store.Close)

	ev := func() []*CalendarEvent {
		return []*CalendarEvent{{
			ID: "evt_1", UserID: "u1", PlanID: "plan_tz", TodoID: "todo_1",
			Title: "Practice chords", Date: "2026-10-05", StartTime: "18:00",
			DurationMin: 60, Status: "scheduled",
		}}
	}

	east := sc.ICS(planInZone(zoneEast), ev())
	west := sc.ICS(planInZone(zoneWest), ev())

	getStart := func(ics, label string) time.Time {
		t.Helper()
		for _, line := range strings.Split(ics, "\r\n") {
			if strings.HasPrefix(line, "DTSTART:") {
				at, err := time.Parse("20060102T150405Z", strings.TrimPrefix(line, "DTSTART:"))
				if err != nil {
					t.Fatalf("%s: unparsable DTSTART %q: %v", label, line, err)
				}
				return at
			}
		}
		t.Fatalf("%s: no DTSTART in ICS", label)
		return time.Time{}
	}

	e := getStart(east, zoneEast)
	w := getStart(west, zoneWest)

	// 18:00 local on the same date in both zones. Auckland is ahead, so its
	// instant must be earlier in UTC.
	if !e.Before(w) {
		t.Errorf("%s 18:00 (%s) should precede %s 18:00 (%s)", zoneEast, e, zoneWest, w)
	}
	locE, _ := time.LoadLocation(zoneEast)
	locW, _ := time.LoadLocation(zoneWest)
	_, offE := time.Date(2026, 10, 5, 18, 0, 0, 0, locE).Zone()
	_, offW := time.Date(2026, 10, 5, 18, 0, 0, 0, locW).Zone()
	wantGap := time.Duration(offE-offW) * time.Second
	if gap := w.Sub(e); gap != wantGap {
		t.Errorf("UTC gap = %v, want %v (the zones' offset difference)", gap, wantGap)
	}

	// The local wall-clock the learner sees must be 18:00 in both.
	if got := e.In(locE).Format("15:04"); got != "18:00" {
		t.Errorf("%s: learner sees %s, want 18:00", zoneEast, got)
	}
	if got := w.In(locW).Format("15:04"); got != "18:00" {
		t.Errorf("%s: learner sees %s, want 18:00", zoneWest, got)
	}
}

// Scheduling must use the plan's zone, so identical plans in two zones produce
// identical local date strings — the dates are floating, the instants are not.
func TestSchedulingIsStableAcrossTimezones(t *testing.T) {
	st := newStore(t.TempDir())
	t.Cleanup(st.Close)
	sc := newScheduler(st)

	datesFor := func(zone string) []string {
		p := planInZone(zone)
		evs := sc.Schedule(p, nil)
		out := []string{}
		for _, e := range evs {
			out = append(out, e.Date+"T"+e.StartTime)
		}
		return out
	}

	east := datesFor(zoneEast)
	west := datesFor(zoneWest)
	if len(east) == 0 {
		t.Fatal("no sessions scheduled")
	}
	if len(east) != len(west) {
		t.Fatalf("session counts differ by zone: %d vs %d", len(east), len(west))
	}
	for i := range east {
		if east[i] != west[i] {
			t.Errorf("session %d differs by zone: %s vs %s — local dates must float", i, east[i], west[i])
		}
	}
}

// A deadline is a calendar date, so it must mean the same day regardless of the
// device that typed it or the host that parsed it.
func TestDeadlineDoesNotShiftBetweenZones(t *testing.T) {
	st := newStore(t.TempDir())
	t.Cleanup(st.Close)
	sc := newScheduler(st)

	for _, zone := range []string{zoneEast, zoneWest, "Asia/Tashkent", "UTC"} {
		p := planInZone(zone)
		p.Deadline = "2026-10-20"
		p.WeeksTotal = 12
		_ = sc.Schedule(p, nil)

		loc := loadLocation(zone)
		dl, ok := parseDateIn(p.Deadline, loc)
		if !ok {
			t.Fatalf("%s: deadline did not parse", zone)
		}
		fin, ok := parseDateIn(p.FinishDate, loc)
		if !ok {
			t.Fatalf("%s: finish date did not parse", zone)
		}
		slip := daysBetween(dl, fin)
		wantMiss := slip > 0
		if p.MissesDeadline != wantMiss {
			t.Errorf("%s: missesDeadline = %v, want %v (slip %d)", zone, p.MissesDeadline, wantMiss, slip)
		}
		if p.MissesDeadline && p.DeadlineSlipDays != slip {
			t.Errorf("%s: slip days = %d, want %d", zone, p.DeadlineSlipDays, slip)
		}
	}
}

// validDate is format-only and must not depend on any zone.
func TestValidDateIsZoneFree(t *testing.T) {
	for _, ok := range []string{"2026-12-01", "2026-01-31", " 2026-06-15 "} {
		if !validDate(ok) {
			t.Errorf("validDate(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "tomorrow", "01-12-2026", "2026-13-01", "2026-02-30", "2026-6-1"} {
		if validDate(bad) {
			t.Errorf("validDate(%q) = true, want false", bad)
		}
	}
}

// The plan records the zone its dates belong to, end to end through the API.
func TestPlanCarriesTheUsersTimezone(t *testing.T) {
	for _, zone := range []string{zoneEast, zoneWest} {
		ts := newTestServer(t)
		c := &client{t: t, base: ts.URL}
		var out map[string]any
		c.do("POST", "/api/session", map[string]any{"lang": "en", "timezone": zone}, &out)
		c.token, _ = out["token"].(string)
		c.user, _ = out["userId"].(string)

		_, planID := c.buildPlanInZone(zone)
		var plan Plan
		c.do("GET", "/api/plan/"+planID, nil, &plan)
		if plan.Timezone != zone {
			t.Errorf("plan timezone = %q, want %q", plan.Timezone, zone)
		}

		resp := c.request("GET", "/api/plan/"+planID+"/ics", nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), "BEGIN:VEVENT") && len(plan.Todos) > 0 {
			// Events only exist after scheduling; this just proves the export
			// renders against the plan without falling over on either zone.
			t.Logf("%s: no events yet (plan not scheduled)", zone)
		}
	}
}
