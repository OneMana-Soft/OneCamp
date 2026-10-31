//go:build integration
// +build integration

package integration_test

// Importing and exporting a calendar (calendar A4) against real Postgres 12
// and object storage: the migration up, down and up; a Google export zip
// through a Content IO job into the person's calendars, with guests and
// their answers, a series with its changes, a Meet link, and nobody told;
// the export over HTTP read back by the codec; and the same zip again
// making nothing twice.
//
// Run: PKGS=./tests/integration/ go test -tags=integration ./tests/integration/ -run 'TestCalendarImport' -v

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	"github.com/akashc777/OneCamp/tests/integration"
)

// importMigration is A4's migration in one direction, found by name: the
// integrator numbers it at merge.
func importMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_calendar_import."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one calendar_import %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCalendarImportMigrationUpDownUp(t *testing.T) {
	up, down := importMigration(t, "up"), importMigration(t, "down")
	env := integration.SetupEnv(t)
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
	guest := func(email string) error {
		return exec(`INSERT INTO event_attendees (event_id, email, role, response, invite_state, added_at) VALUES ($1, $2, 'required', 'accepted', 'imported', NOW())`, event, email)
	}
	external := func(name string) error {
		return exec(`INSERT INTO calendars (id, kind, owner_id, name) VALUES ($1, 'external', $2, $3)`, uuid.New(), user, name)
	}
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			must(exec(step.sql))
		}
		if err := guest("g" + string(rune('a'+i)) + "@example.com"); (err == nil) != step.want {
			t.Fatalf("step %d: an imported guest written: %v, want %v", i, err, step.want)
		}
		name := "Offsites " + string(rune('a'+i))
		must(external(name))
		if err := external(name); (err != nil) != step.want {
			t.Fatalf("step %d: a second calendar of one name refused: %v, want %v", i, err, step.want)
		}
		if !step.want {
			// Two of a name, made while the rule was down, would stop it coming back.
			must(exec(`DELETE FROM calendars WHERE kind = 'external' AND name = $1`, name))
		}
	}
}

func TestCalendarImportOfAGoogleExportOverAJob(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-import-integration")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env, dg := calendarStores(t)
	startMinio(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	who := accessBusiness.Subject{UserUID: sam.UserDgraphInfo.Uid, UserUUID: sam.UserDgraphInfo.Uuid}

	own := strings.Join([]string{"BEGIN:VCALENDAR", "X-WR-CALNAME:" + sam.UserPostgresInfo.EmailID,
		"BEGIN:VEVENT", "UID:review@google.com", "SUMMARY:Weekly design review",
		"DTSTART;TZID=Europe/London:20261006T091500", "DTEND;TZID=Europe/London:20261006T100000",
		"RRULE:FREQ=WEEKLY;BYDAY=TU;COUNT=8", "EXDATE;TZID=Europe/London:20261020T091500",
		"ORGANIZER;CN=Jonas:mailto:jonas@example.com",
		"ATTENDEE;CN=Maya;PARTSTAT=ACCEPTED:mailto:maya@example.com",
		"ATTENDEE;CN=Sam;PARTSTAT=ACCEPTED:mailto:" + sam.UserPostgresInfo.EmailID,
		"X-GOOGLE-CONFERENCE:https://meet.google.com/abc-defg-hij",
		"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT10M", "END:VALARM", "END:VEVENT",
		"BEGIN:VEVENT", "UID:review@google.com", "RECURRENCE-ID;TZID=Europe/London:20261027T091500",
		"SUMMARY:Weekly design review (moved)", "DTSTART;TZID=Europe/London:20261028T140000", "DTEND;TZID=Europe/London:20261028T144500", "END:VEVENT",
		"END:VCALENDAR", ""}, "\r\n")
	offsites := strings.Join([]string{"BEGIN:VCALENDAR", "X-WR-CALNAME:Team offsites",
		"BEGIN:VEVENT", "UID:offsite@google.com", "SUMMARY:Offsite", "DTSTART;VALUE=DATE:20261105", "DTEND;VALUE=DATE:20261107", "END:VEVENT",
		"END:VCALENDAR", ""}, "\r\n")
	data := zipBytes(t, map[string][]byte{"Takeout/Calendar/" + sam.UserPostgresInfo.EmailID + ".ics": []byte(own), "Takeout/Calendar/Team offsites.ics": []byte(offsites)})

	sweepBusiness.Jobs.StartAll(ctx)
	run := func() contentIOBusiness.Job {
		job, err := contentIOBusiness.Pipeline.Import(ctx, who, contentIOBusiness.ImportRequest{Filename: "takeout.zip", Into: contentIOBusiness.IntoCalendar, Size: int64(len(data)), Body: bytes.NewReader(data)})
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		return waitForContentIOJob(ctx, t, who, job.ID)
	}
	job := run()
	if job.Status != contentIOBusiness.JobDone || len(job.Created) != 2 || len(job.Skipped) != 0 {
		t.Fatalf("the import ended %+v", job)
	}

	// In the store: the series in Sam's own calendar with its moved
	// Tuesday, the offsite in a calendar of its name, guests as imported.
	var calendarKind, notes string
	var exdates int
	if err := env.PG.QueryRow(`SELECT c.kind, e.notes, cardinality(e.exdates) FROM events e JOIN calendars c ON c.id = e.calendar_id
		WHERE e.ical_uid = 'review@google.com' AND e.series_id IS NULL`).Scan(&calendarKind, &notes, &exdates); err != nil {
		t.Fatal(err)
	}
	if calendarKind != "personal" || !strings.HasPrefix(notes, "Join: https://meet.google.com/abc-defg-hij") || exdates != 1 {
		t.Fatalf("series: %s %q %d", calendarKind, notes, exdates)
	}
	var overrides, guests, offsiteCalendars int
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(env.PG.QueryRow(`SELECT count(*) FROM events o JOIN events s ON s.id = o.series_id WHERE s.ical_uid = 'review@google.com'`).Scan(&overrides))
	must(env.PG.QueryRow(`SELECT count(*) FROM event_attendees a JOIN events e ON e.id = a.event_id
		WHERE e.ical_uid = 'review@google.com' AND a.invite_state = 'imported' AND a.user_id IS NULL`).Scan(&guests))
	must(env.PG.QueryRow(`SELECT count(*) FROM events e JOIN calendars c ON c.id = e.calendar_id
		WHERE e.ical_uid = 'offsite@google.com' AND c.kind = 'external' AND c.name = 'Team offsites'`).Scan(&offsiteCalendars))
	if overrides != 1 || guests != 2 || offsiteCalendars != 1 {
		t.Fatalf("overrides %d, imported guests %d (Jonas and Maya, never Sam), offsite calendars %d", overrides, guests, offsiteCalendars)
	}
	var reminders int
	must(env.PG.QueryRow(`SELECT count(*) FROM event_reminders r JOIN events e ON e.id = r.event_id WHERE e.ical_uid = 'review@google.com'`).Scan(&reminders))
	if reminders == 0 {
		t.Fatal("the alarm isn't Sam's reminder")
	}

	// The export reads back as the same events, the call link included.
	rec := call(t, eventController.ExportCalendar, http.MethodGet, "/calendar/events/export", sam, "", nil)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/calendar") {
		t.Fatalf("export: %d %s", rec.Code, rec.Body.String())
	}
	cal, err := ICS.Read(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, ev := range cal.Events {
		seen[ev.UID]++
		if ev.UID == "review@google.com" && ev.RecurrenceID == nil && ev.Conference != "https://meet.google.com/abc-defg-hij" {
			t.Errorf("the export lost the call link: %+v", ev)
		}
	}
	if seen["review@google.com"] != 2 || seen["offsite@google.com"] != 1 {
		t.Fatalf("exported %v", seen)
	}

	// The same zip again makes nothing twice.
	again := run()
	if again.Status != contentIOBusiness.JobDone || len(again.Created) != 0 || again.Unchanged != 2 {
		t.Fatalf("again: %+v", again)
	}
}
