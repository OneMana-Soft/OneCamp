//go:build integration
// +build integration

package integration_test

// Calendars that belong to something, and calendars shared (calendar A6),
// against real Postgres 12: the migration up, down and up; a project's
// calendar made once however many ask at once; a person's own calendars, the
// personal one first; a calendar shared, its level changed and the share
// stopped; a rename that keeps the colour; and what is on a project's
// calendar in a window, and next on it, read by the calendar rather than by
// who made it.
//
// Run: go test -tags=integration ./tests/integration/ -run '^(TestCalendarContainersMigrationUpDownUp|TestCalendarContainers)$' -v

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/akashc777/OneCamp/tests/integration"
)

func containersMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_calendar_containers."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one calendar_containers %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCalendarContainersMigrationUpDownUp(t *testing.T) {
	up, down := containersMigration(t, "up"), containersMigration(t, "down")
	if strings.Contains(up, "gen_random_uuid") {
		t.Fatal("ids are made in Go")
	}
	env := integration.SetupEnv(t)
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		for _, name := range []string{"calendar_shares", "calendars_one_per_container_idx", "events_by_calendar_idx"} {
			if got := tableExists(t, env.PG, name); got != step.want {
				t.Fatalf("step %d: %s exists = %v", i, name, got)
			}
		}
		// events_by_calendar_idx leads with calendar_id, so it serves the
		// foreign key too: the foreign-key sweep's single-column index is a
		// second copy while it is up, and comes back when it goes down.
		if got := tableExists(t, env.PG, "idx_fk_events_calendar_id"); got == step.want {
			t.Fatalf("step %d: idx_fk_events_calendar_id exists = %v while events_by_calendar_idx exists = %v", i, got, step.want)
		}
		for _, column := range []string{"container_kind", "container_id"} {
			if got := columnExists(t, env, "calendars", column); got != step.want {
				t.Fatalf("step %d: calendars.%s exists = %v", i, column, got)
			}
		}
	}
}

func TestCalendarContainers(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	db := env.PG
	// The models run on the server's pool (models/pgexec): this test's database.
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	person := func(name string) uuid.UUID {
		id := uuid.New()
		_, err := db.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name)
		must(err)
		return id
	}
	sam, ria := person("sam"), person("ria")

	// A project's calendar is made once: asked again, and asked by many at
	// once, it is the same one.
	project := "project-" + uuid.NewString()
	first, err := models.ContainerCalendar(ctx, "project", project, uuid.New(), "Asia/Kolkata")
	must(err)
	again, err := models.ContainerCalendar(ctx, "project", project, uuid.New(), "UTC")
	if err != nil || again != first {
		t.Fatalf("asked again: %s %v, want %s", again, err, first)
	}
	busy := "project-" + uuid.NewString()
	var wg sync.WaitGroup
	got := make([]uuid.UUID, 8)
	errs := make([]error, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = models.ContainerCalendar(ctx, "project", busy, uuid.New(), "UTC")
		}(i)
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i] != got[0] {
			t.Fatalf("at once, %d: %s %v, want %s", i, got[i], errs[i], got[0])
		}
	}
	var n int
	must(db.QueryRow(`SELECT count(*) FROM calendars WHERE container_kind = 'project' AND container_id = $1`, busy).Scan(&n))
	if n != 1 {
		t.Fatalf("calendars made at once: %d", n)
	}

	// Read back: the project's has no owner, its zone and its container.
	samPersonal, err := models.PersonalCalendar(ctx, db, sam, uuid.New(), "Europe/London")
	must(err)
	cals, err := models.CalendarsByID(ctx, []uuid.UUID{first, samPersonal})
	must(err)
	byID := map[uuid.UUID]models.Calendar{}
	for _, c := range cals {
		byID[c.ID] = c
	}
	if c := byID[first]; c.Kind != "project" || c.OwnerID != nil || c.TZ != "Asia/Kolkata" || c.ContainerKind != "project" || c.ContainerID != project || c.Deleted {
		t.Fatalf("the project's calendar: %+v", c)
	}
	if c := byID[samPersonal]; c.Kind != "personal" || c.OwnerID == nil || *c.OwnerID != sam || c.ContainerKind != "" {
		t.Fatalf("sam's own: %+v", c)
	}
	projects, err := models.ContainerCalendars(ctx, "project", 100)
	must(err)
	seen := map[uuid.UUID]bool{}
	for _, c := range projects {
		seen[c.ID] = true
	}
	if !seen[first] || !seen[got[0]] || seen[samPersonal] {
		t.Fatalf("project calendars: %+v", projects)
	}

	// A person's own: the personal one first, then what an import made; never
	// a project's.
	imported, err := models.ImportedCalendar(ctx, sam, "Family", uuid.New(), "UTC")
	must(err)
	own, err := models.OwnCalendars(ctx, sam)
	must(err)
	if len(own) != 2 || own[0].ID != samPersonal || own[1].ID != imported {
		t.Fatalf("sam's own calendars: %+v", own)
	}
	riaPersonal, err := models.PersonalCalendar(ctx, db, ria, uuid.New(), "UTC")
	must(err)
	personal, err := models.PersonalCalendarsOf(ctx, []uuid.UUID{sam, ria})
	must(err)
	if len(personal) != 2 {
		t.Fatalf("personal calendars of both: %+v", personal)
	}

	// Sam shares his calendar with Ria: details, then edit, then not at all.
	// A level the calendar doesn't know is refused.
	must(models.SetShare(ctx, samPersonal, ria, sam, models.ShareDetails))
	levels, err := models.SharesFor(ctx, ria, []uuid.UUID{samPersonal, first, riaPersonal})
	if err != nil || len(levels) != 1 || levels[samPersonal] != models.ShareDetails {
		t.Fatalf("ria's levels: %v %v", levels, err)
	}
	must(models.SetShare(ctx, samPersonal, ria, sam, models.ShareEdit))
	shares, err := models.SharesOf(ctx, samPersonal)
	if err != nil || len(shares) != 1 || shares[0].PersonID != ria || shares[0].Level != models.ShareEdit {
		t.Fatalf("sam's calendar is shared with: %+v %v", shares, err)
	}
	with, err := models.SharedWith(ctx, ria)
	if err != nil || len(with) != 1 || with[0].ID != samPersonal {
		t.Fatalf("shared with ria: %+v %v", with, err)
	}
	if err := models.SetShare(ctx, samPersonal, ria, sam, "owner"); err == nil {
		t.Fatal("a level the calendar doesn't know was kept")
	}
	must(models.SetShare(ctx, samPersonal, ria, sam, ""))
	if shares, _ := models.SharesOf(ctx, samPersonal); len(shares) != 0 {
		t.Fatalf("after the share stopped: %+v", shares)
	}
	// A calendar in the trash is shared with no one.
	must(models.SetShare(ctx, imported, ria, sam, models.ShareDetails))
	_, err = db.Exec(`UPDATE calendars SET deleted_at = NOW() WHERE id = $1`, imported)
	must(err)
	if with, _ := models.SharedWith(ctx, ria); len(with) != 0 {
		t.Fatalf("a trashed calendar is still shared: %+v", with)
	}
	if cals, _ := models.CalendarsByID(ctx, []uuid.UUID{imported}); len(cals) != 1 || !cals[0].Deleted {
		t.Fatalf("the trashed calendar, read by id: %+v", cals)
	}

	// A rename keeps the colour; a new colour keeps the name.
	name, hue := "Launch", "teal"
	must(models.UpdateCalendar(ctx, first, &name, nil))
	must(models.UpdateCalendar(ctx, first, nil, &hue))
	if cals, _ := models.CalendarsByID(ctx, []uuid.UUID{first}); len(cals) != 1 || cals[0].Name != "Launch" || cals[0].Hue != "teal" {
		t.Fatalf("renamed and recoloured: %+v", cals)
	}

	// What is on the project's calendar, whoever made it: in the window, a
	// series that reaches it, never one in the trash, past it or on another
	// calendar.
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	event := func(calendar, by uuid.UUID, start time.Time, rule string) uuid.UUID {
		id := uuid.New()
		_, err := db.Exec(`INSERT INTO events (id, calendar_id, created_by, ical_uid, start_at, end_at, rrule) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			id, calendar, by, id.String()+"@onecamp", start, start.Add(time.Hour), rule)
		must(err)
		return id
	}
	inside := event(first, ria, day.Add(10*time.Hour), "")
	weekly := event(first, sam, day.AddDate(0, 0, -14).Add(9*time.Hour), "FREQ=WEEKLY")
	later := event(first, sam, day.AddDate(0, 0, 9), "")
	trashed := event(first, sam, day.Add(12*time.Hour), "")
	_, err = db.Exec(`UPDATE events SET deleted_at = NOW() WHERE id = $1`, trashed)
	must(err)
	elsewhere := event(samPersonal, sam, day.Add(11*time.Hour), "")
	allDay := uuid.New()
	_, err = db.Exec(`INSERT INTO events (id, calendar_id, created_by, ical_uid, start_date, end_date) VALUES ($1, $2, $3, $4, '2026-11-03', '2026-11-04')`,
		allDay, first, ria, allDay.String()+"@onecamp")
	must(err)
	w := models.Window{From: day, To: day.AddDate(0, 0, 7), FromDate: "2026-11-01", ToDate: "2026-11-10"}
	on, err := models.RangeOnCalendars(ctx, db, []uuid.UUID{first}, w, 50)
	must(err)
	found := map[uuid.UUID]bool{}
	for _, e := range on {
		found[e.ID] = true
	}
	if len(found) != 3 || !found[inside] || !found[weekly] || !found[allDay] || found[later] || found[trashed] || found[elsewhere] {
		t.Fatalf("on the project's calendar this week: %v (inside %s, weekly %s, all day %s)", found, inside, weekly, allDay)
	}
	both, err := models.RangeOnCalendars(ctx, db, []uuid.UUID{first, samPersonal}, w, 50)
	if err != nil || len(both) != 4 {
		t.Fatalf("on both calendars: %d %v", len(both), err)
	}

	// Next on it from the day: the series (it has no end), what comes, never
	// what is over, cancelled or in the trash; at most the limit, by start.
	over := event(first, sam, day.AddDate(0, 0, -3), "")
	cancelled := event(first, sam, day.AddDate(0, 0, 1), "")
	_, err = db.Exec(`UPDATE events SET status = 'cancelled' WHERE id = $1`, cancelled)
	must(err)
	next, err := models.NextOf(ctx, db, []uuid.UUID{first}, day, 10)
	must(err)
	var order []uuid.UUID
	for _, e := range next {
		order = append(order, e.ID)
	}
	if len(order) != 4 || order[0] != weekly || order[1] != inside || order[2] != allDay || order[3] != later {
		t.Fatalf("next on the project's calendar: %v (weekly %s, inside %s, all day %s, later %s; not over %s or cancelled %s)", order, weekly, inside, allDay, later, over, cancelled)
	}
	if next, _ := models.NextOf(ctx, db, []uuid.UUID{first}, day, 2); len(next) != 2 {
		t.Fatalf("next, two of them: %d", len(next))
	}
}
