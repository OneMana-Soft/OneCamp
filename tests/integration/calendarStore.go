//go:build integration
// +build integration

package integration

// The calendar's two stores, for tests (business/Calendar, A1): a test that
// pins what the calendar does runs once with the events in the two old stores
// (the graph's Event nodes and calendar_events) and once in the one store
// (CALENDAR_STORE=postgres), and must pass alike, which is how the switch is
// proven before it happens.

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// CalendarStores are the two: "graph", the old stores, and "postgres", the
// one store.
var CalendarStores = []string{"graph", "postgres"}

// ForEachCalendarStore runs fn once per store, as a subtest named for it,
// with CALENDAR_STORE set for it (helpers.CalendarInPostgres says which). fn
// sets up its own databases: a fresh one in the one store holds nothing to
// move, so MarkCalendarMoved says so.
func ForEachCalendarStore(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	for _, store := range CalendarStores {
		t.Run(store, func(t *testing.T) {
			t.Setenv("CALENDAR_STORE", store)
			fn(t)
		})
	}
}

// MarkCalendarMoved records in db that the move into the one store is done,
// so the calendar answers at once: a fresh database has nothing to move.
func MarkCalendarMoved(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO system_configs (key, value, updated_at) VALUES ('calendar_store_moved', 'done: a fresh test database', NOW())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
}

// StoreEvent is one event a test writes straight into the one store, as the
// old tests wrote calendar_events rows.
type StoreEvent struct {
	By    uuid.UUID
	Kind  string // "event", "focus", "out_of_office"…; "" is "event"
	Title string
	// Timed (Start, End) or all-day (StartDate, EndDate, "2026-10-16", the end
	// the day after the last) in Zone ("" is UTC).
	Start, End         time.Time
	StartDate, EndDate string
	Zone               string
	Deleted            bool
}

// InsertStoreEvent writes e, and its organiser's calendar when they have
// none, and answers its id.
func InsertStoreEvent(t *testing.T, db *sql.DB, e StoreEvent) uuid.UUID {
	t.Helper()
	if e.Kind == "" {
		e.Kind = "event"
	}
	if e.Zone == "" {
		e.Zone = "UTC"
	}
	if _, err := db.Exec(`INSERT INTO calendars (id, kind, owner_id, tz) VALUES ($1, 'personal', $2, $3)
		ON CONFLICT (owner_id) WHERE kind = 'personal' AND deleted_at IS NULL DO NOTHING`, uuid.New(), e.By, e.Zone); err != nil {
		t.Fatal(err)
	}
	var calendar uuid.UUID
	if err := db.QueryRow(`SELECT id FROM calendars WHERE owner_id = $1 AND kind = 'personal' AND deleted_at IS NULL`, e.By).Scan(&calendar); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	var startAt, endAt, deleted any
	var startDate, endDate any
	if e.StartDate != "" {
		startDate, endDate = e.StartDate, e.EndDate
	} else {
		startAt, endAt = e.Start, e.End
	}
	if e.Deleted {
		deleted = time.Now()
	}
	if _, err := db.Exec(`INSERT INTO events (id, calendar_id, kind, title, start_at, end_at, start_date, end_date, tz, created_by, ical_uid, deleted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::date, $8::date, $9, $10, $11, $12)`,
		id, calendar, e.Kind, e.Title, startAt, endAt, startDate, endDate, e.Zone, e.By, id.String()+"@onecamp", deleted); err != nil {
		t.Fatal(err)
	}
	return id
}
