//go:build integration

package integration_test

// meet-02 (2): an event call's meeting knows its event (call_meetings.event_id),
// so what the call makes lands on the event. The migration fills the event
// calls made before it from the calendar store, which holds each event's call
// by its room.

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func callMeetingsEventMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_call_meetings_event."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one call_meetings_event %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The meeting's event migration goes down and up again on Postgres 12. Up
// again, each event call made before it knows the event holding its room:
// the first to hold it, so a series split in two keeps its call on the series
// that made it, and never one of its own instances. A call taken off (no
// event holds its room any more), an instant meeting and a personal room know
// no event, and an event purged for good leaves its meeting without one.
func TestCallMeetingsEventMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	calendar, owner := uuid.New(), uuid.New()
	exec(`INSERT INTO calendars (id, kind, name) VALUES ($1, 'team', 'Calls')`, calendar)
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	made := start.Add(-72 * time.Hour)
	event := func(room string, created time.Time, series *uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		var recurrence *time.Time
		if series != nil {
			recurrence = &start
		}
		exec(`INSERT INTO events (id, calendar_id, title, start_at, end_at, call_room, ical_uid, series_id, recurrence_id, created_at)
			VALUES ($1, $2, 'Weekly sync', $3, $4, $5, $6, $7, $8, $9)`,
			id, calendar, start, start.Add(time.Hour), room, id.String()+"@onecamp", series, recurrence, created)
		return id
	}
	meeting := func(id, kind string) {
		t.Helper()
		exec(`INSERT INTO call_meetings (id, kind, owner, lobby, created_at) VALUES ($1, $2, $3, 'guests', NOW())`, id, kind, owner)
	}
	eventOf := func(id string) uuid.NullUUID {
		t.Helper()
		var e uuid.NullUUID
		if err := env.PG.QueryRow(`SELECT event_id FROM call_meetings WHERE id = $1`, id).Scan(&e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	defer func() {
		exec(`DELETE FROM call_meetings WHERE owner = $1`, owner)
		exec(`DELETE FROM calendars WHERE id = $1`, calendar)
	}()

	if _, err := env.PG.Exec(callMeetingsEventMigration(t, "down")); err != nil {
		t.Fatalf("down: %v", err)
	}
	var none sql.NullString
	if err := env.PG.QueryRow(`SELECT event_id::text FROM call_meetings LIMIT 1`).Scan(&none); err == nil || err == sql.ErrNoRows {
		t.Fatalf("down: call_meetings still has event_id (%v)", err)
	}

	single := event("meet-ev-single", made, nil)
	series := event("meet-ev-split", made, nil)
	// One of the series' own instances holds its call too (setCallRoom gives
	// it them), made before the series to show the fill never takes it.
	event("meet-ev-split", made.Add(-time.Hour), &series)
	// "This and following" made a series of the rest, holding the same call.
	event("meet-ev-split", made.Add(time.Hour), nil)
	event("meet-m-instant", made, nil)
	for id, kind := range map[string]string{"ev-single": "event", "ev-split": "event", "ev-off": "event", "m-instant": "instant", "room": "personal"} {
		meeting(id, kind)
	}

	if _, err := env.PG.Exec(callMeetingsEventMigration(t, "up")); err != nil {
		t.Fatalf("up again: %v", err)
	}
	for id, want := range map[string]uuid.NullUUID{
		"ev-single": {UUID: single, Valid: true},
		"ev-split":  {UUID: series, Valid: true},
		"ev-off":    {},
		"m-instant": {},
		"room":      {},
	} {
		if got := eventOf(id); got != want {
			t.Errorf("up: %s's event %v, want %v", id, got, want)
		}
	}
	if _, err := env.PG.Exec(`UPDATE call_meetings SET event_id = $2 WHERE id = $1`, "room", uuid.New()); err == nil {
		t.Error("up: a meeting took an event that isn't there")
	}

	exec(`DELETE FROM events WHERE id = $1`, single)
	if got := eventOf("ev-single"); got.Valid {
		t.Errorf("a purged event's meeting still names it: %v", got)
	}
}
