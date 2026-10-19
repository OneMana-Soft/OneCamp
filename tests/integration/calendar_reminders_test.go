//go:build integration
// +build integration

package integration_test

// Reminders (calendar A5) in the one store, against real Postgres 12: the
// migration up, down and up; a person's reminders over HTTP; each person's
// next reminder planned from the event's own Resource Event, one waiting job
// per person and event in the scheduler's queue; a due one sent once, as a
// listed notice with no inbox row, and nothing planned after the last. The
// service's own rules are pinned in business/Calendar/reminders_test.go;
// these hold the SQL and the queue to them.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestCalendarReminders' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	"github.com/akashc777/OneCamp/tests/integration"
)

// remindersMigration is calendar A5's migration in one direction, found by
// name: the integrator numbers it at merge.
func remindersMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_calendar_reminders."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one calendar_reminders %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The migration goes up, down and up again on Postgres 12; a person has at
// most five reminders for an event, and one waiting reminder job of a kind.
func TestCalendarRemindersMigrationUpDownUp(t *testing.T) {
	up, down := remindersMigration(t, "up"), remindersMigration(t, "down")
	if strings.Contains(up, "gen_random_uuid") {
		t.Fatal("ids are made in Go")
	}
	env := integration.SetupEnv(t)
	indexExists := func(name string) bool {
		var ok bool
		if err := env.PG.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		if got := tableExists(t, env.PG, "event_reminders"); got != step.want {
			t.Fatalf("step %d: event_reminders exists = %v", i, got)
		}
		if got := indexExists("scheduled_jobs_calendar_reminder_idx"); got != step.want {
			t.Fatalf("step %d: the one-job index exists = %v", i, got)
		}
	}
	user, calendar, event := uuid.New(), uuid.New(), uuid.New()
	exec := func(q string, args ...any) error { _, err := env.PG.Exec(q, args...); return err }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'u', 'u', NOW(), NOW())`, user, user.String()+"@example.test"))
	must(exec(`INSERT INTO calendars (id, owner_id) VALUES ($1, $2)`, calendar, user))
	must(exec(`INSERT INTO events (id, calendar_id, created_by, ical_uid, start_at, end_at) VALUES ($1, $2, $3, $4, NOW(), NOW() + interval '1 hour')`,
		event, calendar, user, event.String()+"@onecamp"))
	if err := exec(`INSERT INTO event_reminders (event_id, user_id, minutes, updated_at) VALUES ($1, $2, '{1,2,3,4,5,6}', NOW())`, event, user); err == nil {
		t.Error("six reminders for one event")
	}
	must(exec(`INSERT INTO event_reminders (event_id, user_id, minutes, updated_at) VALUES ($1, $2, '{0,10,30}', NOW())`, event, user))
	job := func(status string) error {
		return exec(`INSERT INTO scheduled_jobs (id, job_type, user_uuid, payload, run_at, status) VALUES ($1, 'calendar_reminder', $2, $3, NOW(), $4)`,
			uuid.New(), user, `{"event_id":"`+event.String()+`","kind":"next"}`, status)
	}
	must(job("pending"))
	if err := job("pending"); err == nil {
		t.Error("two waiting reminders of a kind for one person and event")
	}
	must(job("done"))
	// The event goes, and its reminders with it.
	must(exec(`DELETE FROM events WHERE id = $1`, event))
	var left int
	must(env.PG.QueryRow(`SELECT count(*) FROM event_reminders WHERE event_id = $1`, event).Scan(&left))
	if left != 0 {
		t.Errorf("%d reminders outlived their event", left)
	}
}

// waitingReminder is when person's next reminder for event fires, and its
// minutes, once it is planned (the plan follows the event's Resource Event).
func waitingReminder(t *testing.T, env *integration.Env, person uuid.UUID, event string) (time.Time, int, bool) {
	t.Helper()
	var at time.Time
	var payload string
	err := env.PG.QueryRow(`SELECT run_at, payload FROM scheduled_jobs WHERE job_type = 'calendar_reminder' AND status = 'pending'
		AND user_uuid = $1 AND payload->>'event_id' = $2 AND payload->>'kind' = 'next'`, person, event).Scan(&at, &payload)
	if err != nil {
		return time.Time{}, 0, false
	}
	var p struct {
		Minutes int `json:"minutes"`
	}
	_ = json.Unmarshal([]byte(payload), &p)
	return at.UTC(), p.Minutes, true
}

func TestCalendarRemindersOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	ctx := context.Background()
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	samID, mayaID := sam.UserPostgresInfo.Id, maya.UserPostgresInfo.Id

	start := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Minute)
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Design review","start":{"at":"`+rfc(start)+`","zone":"Europe/London"},"end":{"at":"`+rfc(start.Add(time.Hour))+`"},`+
			`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	e := dataEvent(t, body)

	// Sam organises it: 10 minutes before, by default. Maya hasn't answered.
	eventually(t, "Sam's reminder to be planned", func() bool {
		at, m, ok := waitingReminder(t, env, samID, e.ID)
		return ok && at.Equal(start.Add(-10*time.Minute)) && m == 10
	})
	if _, _, ok := waitingReminder(t, env, mayaID, e.ID); ok {
		t.Error("Maya hasn't answered, and is reminded by default")
	}

	// Maya sets her own: half an hour before.
	code, body = v2(t, eventController.SetCalendarReminders, http.MethodPut, "/calendar/events/"+e.ID+"/reminders", maya, `{"minutes":[30]}`, e.ID)
	var rem struct {
		Minutes []int `json:"minutes"`
		Default bool  `json:"default"`
	}
	if code != 200 || json.Unmarshal(body["data"], &rem) != nil || rem.Default || !slices.Equal(rem.Minutes, []int{30}) {
		t.Fatalf("Maya's reminders: %d %v", code, body)
	}
	if at, _, ok := waitingReminder(t, env, mayaID, e.ID); !ok || !at.Equal(start.Add(-30*time.Minute)) {
		t.Errorf("Maya's next: %v %v", at, ok)
	}
	code, body = v2(t, eventController.GetCalendarReminders, http.MethodGet, "/calendar/events/"+e.ID+"/reminders", sam, "", e.ID)
	if code != 200 || json.Unmarshal(body["data"], &rem) != nil || !rem.Default || !slices.Equal(rem.Minutes, []int{10}) {
		t.Errorf("Sam's reminders: %d %v", code, body)
	}

	// Sam chooses none, then goes back to the default.
	if code, body := v2(t, eventController.SetCalendarReminders, http.MethodPut, "/calendar/events/"+e.ID+"/reminders", sam, `{"minutes":[]}`, e.ID); code != 200 {
		t.Fatalf("none: %d %v", code, body)
	}
	if _, _, ok := waitingReminder(t, env, samID, e.ID); ok {
		t.Error("Sam chose none and is still reminded")
	}
	if code, body := v2(t, eventController.SetCalendarReminders, http.MethodPut, "/calendar/events/"+e.ID+"/reminders", sam, `{"minutes":null}`, e.ID); code != 200 {
		t.Fatalf("the default again: %d %v", code, body)
	}
	if at, _, ok := waitingReminder(t, env, samID, e.ID); !ok || !at.Equal(start.Add(-10*time.Minute)) {
		t.Errorf("Sam's default again: %v %v", at, ok)
	}
	if code, _ := v2(t, eventController.SetCalendarReminders, http.MethodPut, "/calendar/events/"+e.ID+"/reminders", sam, `{"minutes":[1,2,3,4,5,6]}`, e.ID); code != http.StatusBadRequest {
		t.Errorf("six reminders: %d", code)
	}

	// Due now, Sam's reminder is sent once by the queue, as a listed notice
	// (pushed, with no inbox row), and nothing is left waiting after the last.
	if _, err := env.PG.Exec(`UPDATE scheduled_jobs SET run_at = NOW() - interval '1 second' WHERE job_type = 'calendar_reminder'
		AND status = 'pending' AND user_uuid = $1 AND payload->>'event_id' = $2`, samID, e.ID); err != nil {
		t.Fatal(err)
	}
	if ran := schedulerBusiness.DrainDueJobs(ctx, "calendar-reminders-test"); ran < 1 {
		t.Fatalf("the queue ran %d jobs", ran)
	}
	var reason string
	eventually(t, "Sam's reminder notice", func() bool {
		return env.PG.QueryRow(`SELECT reason FROM resource_notices WHERE user_uuid = $1 AND kind = 'calendar.reminder' AND resource_uuid = $2`,
			samID, e.ID).Scan(&reason) == nil
	})
	if reason != "listed" {
		t.Errorf("the reminder's row: %q, want a listed one (no inbox item)", reason)
	}
	if _, _, ok := waitingReminder(t, env, samID, e.ID); ok {
		t.Error("the same reminder was planned again after it fired")
	}

	// Snoozed for five minutes, it waits once more, as a snooze.
	code, body = v2(t, eventController.SnoozeCalendarReminder, http.MethodPost, "/calendar/events/"+e.ID+"/snooze", sam, `{"until":"5m"}`, e.ID)
	if code != 200 {
		t.Fatalf("snooze: %d %v", code, body)
	}
	var snoozes int
	if err := env.PG.QueryRow(`SELECT count(*) FROM scheduled_jobs WHERE job_type = 'calendar_reminder' AND status = 'pending'
		AND user_uuid = $1 AND payload->>'event_id' = $2 AND payload->>'kind' = 'snooze'`, samID, e.ID).Scan(&snoozes); err != nil || snoozes != 1 {
		t.Errorf("%d snoozes waiting (%v)", snoozes, err)
	}

	// Deleted, the event reminds nobody.
	if code, _ := v2(t, eventController.DeleteCalendarEvent, http.MethodDelete, "/calendar/events/"+e.ID, sam, "", e.ID); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	eventually(t, "the deleted event's reminders to be cancelled", func() bool {
		var n int
		_ = env.PG.QueryRow(`SELECT count(*) FROM scheduled_jobs WHERE job_type = 'calendar_reminder' AND status = 'pending' AND payload->>'event_id' = $1`, e.ID).Scan(&n)
		return n == 0
	})
}
