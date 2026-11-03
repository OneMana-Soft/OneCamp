//go:build integration
// +build integration

package integration_test

// A calendar a person subscribed to and then unsubscribed from is kept, with
// who unsubscribed and when (V-184 head 5c, the owner's rule that every
// delete of a business record is soft).
//
// Unsubscribing DELETEd the calendar and, by cascade, every event and share
// on it, so nobody could say afterwards that the person had read it, whom it
// was shared with, or when it went. Now the calendar stays, marked by the
// person; the address's events on it are forgotten (they are the address's,
// mirrored from its file) and anything else on it stays with it; its shares
// are marked by the person; and no read sees it: the person's subscriptions
// and calendars, the calendars shared with the sharee, a lookup by id and the
// refresher's list. The lookup by id is the access rules' answer, which every
// route that takes a calendar's id asks: a calendar unsubscribed from answers
// none, deleted, with what the person had before (I9). Someone else can't
// unsubscribe it, and subscribing to the same address again makes a new
// calendar beside the one kept.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAnUnsubscribedCalendarIsKeptWithWhoAndWhen|TestCalendarsSoftDeleteMigrationUpDownUp' -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	calendarModel "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnUnsubscribedCalendarIsKeptWithWhoAndWhen(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	ctx := context.Background()
	sam, ria := seedCalendarPerson(t, env.PG, dg, "Sam"), seedCalendarPerson(t, env.PG, dg, "Ria")
	samID, riaID := sam.UserPostgresInfo.Id, ria.UserPostgresInfo.Id
	link, err := calendarModel.EnsureSyncLink(ctx, uuid.New(), samID, "ics")
	if err != nil {
		t.Fatal(err)
	}
	const remote = "digest-of-the-team-address"

	// subscribe makes Sam a calendar read from the team's address, as
	// Subscribe does once it has read the file: the calendar, and its row in
	// the ledger under his "ics" link.
	subscribe := func() uuid.UUID {
		t.Helper()
		cal := uuid.New()
		if err := calendarModel.SubscribedCalendar(ctx, cal, samID, "Team", "", "UTC"); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		added, err := calendarModel.AddSubscription(ctx, calendarModel.Subscription{
			SyncCalendar: calendarModel.SyncCalendar{LinkID: link.ID, RemoteID: remote, Name: "Team", CalendarID: &cal},
			PersonID:     samID, SourceSealed: "sealed", ReadAt: &now})
		if err != nil || !added {
			t.Fatalf("Sam subscribes to the team's address: added %v, %v", added, err)
		}
		return cal
	}
	event := func(cal uuid.UUID, source string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO events (id, calendar_id, title, start_at, end_at, ical_uid, source, created_by)
			VALUES ($1, $2, 'Review', NOW(), NOW() + interval '1 hour', $3, $4, $5)`, id, cal, id.String(), source, samID); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// listed says whether a list route answers cal among its data, by field.
	listed := func(h http.HandlerFunc, path string, who *userModels.UserInfo, field string, cal uuid.UUID) bool {
		t.Helper()
		rec := call(t, h, http.MethodGet, path, who, "", nil)
		var body struct {
			Data []map[string]any `json:"data"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("GET %s: HTTP %d %s", path, rec.Code, rec.Body.String())
		}
		for _, d := range body.Data {
			if d[field] == cal.String() {
				return true
			}
		}
		return false
	}
	// decided is the access rules' answer for Sam on a calendar by its id:
	// what every route that takes a calendar's id asks first. A calendar read
	// from an address is his to see and nobody's to manage, so it is never his
	// shares lookup's (that is for its managers): the rules are the lookup.
	decided := func(cal uuid.UUID) accessBusiness.Decision {
		t.Helper()
		d, err := accessBusiness.Rules.Resolve(ctx, accessBusiness.Subject{UserUID: sam.UserDgraphInfo.Uid, UserUUID: samID.String()},
			accessBusiness.Resource{Kind: calendarBusiness.KindCalendar, UUID: cal.String()})
		if err != nil {
			t.Fatalf("the access rules on Sam's calendar %s: %v", cal, err)
		}
		return d
	}
	// seen is every read of the calendar: Sam's subscriptions and calendars,
	// the calendars shared with Ria, Sam's lookup of it by id, and the
	// refresher's list.
	type views struct{ subscriptions, calendars, sharedWithRia, byID, due bool }
	seen := func(cal uuid.UUID) views {
		t.Helper()
		var v views
		v.subscriptions = listed(eventController.ListSubscriptions, "/calendar/subscriptions", sam, "calendar_id", cal)
		v.calendars = listed(eventController.ListCalendars, "/calendar/calendars", sam, "id", cal)
		with, err := calendarModel.SharedWith(ctx, riaID)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range with {
			v.sharedWithRia = v.sharedWithRia || c.ID == cal
		}
		v.byID = decided(cal).Level > accessBusiness.LevelNone
		due, err := calendarModel.DueSubscriptions(ctx, time.Now().Add(time.Hour), 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range due {
			v.due = v.due || (d.CalendarID != nil && *d.CalendarID == cal)
		}
		return v
	}
	unsubscribe := func(who *userModels.UserInfo, cal uuid.UUID) int {
		t.Helper()
		return call(t, eventController.Unsubscribe, http.MethodDelete, "/calendar/subscriptions/"+cal.String(), who, "",
			map[string]string{"calendarId": cal.String()}).Code
	}

	// Sam subscribes, the address brings two events, something else is on the
	// calendar too, and he shares it with Ria.
	cal := subscribe()
	brought := []uuid.UUID{event(cal, "subscribed"), event(cal, "subscribed")}
	other := event(cal, "moved")
	if err := calendarModel.SetShare(ctx, cal, riaID, samID, calendarModel.ShareDetails); err != nil {
		t.Fatal(err)
	}
	if v := seen(cal); v != (views{true, true, true, true, true}) {
		t.Fatalf("before unsubscribing, the reads see %+v; want every one to see it", v)
	}
	if d := decided(cal); d.Level != accessBusiness.LevelView || !d.Owner || d.Deleted {
		t.Fatalf("before unsubscribing, the rules give Sam %v (owner %v, deleted %v); want view, as its owner, live", d.Level, d.Owner, d.Deleted)
	}

	// Ria can't unsubscribe Sam's calendar.
	if code := unsubscribe(ria, cal); code != http.StatusNotFound {
		t.Fatalf("Ria unsubscribes Sam's calendar: HTTP %d; want 404", code)
	}
	// Sam unsubscribes.
	if code := unsubscribe(sam, cal); code != http.StatusNoContent {
		t.Fatalf("Sam unsubscribes: HTTP %d; want 204", code)
	}

	// The calendar is kept, as it was, marked by Sam.
	var (
		kept bool
		by   uuid.NullUUID
		name string
	)
	if err := env.PG.QueryRow(`SELECT deleted_at IS NOT NULL, deleted_by, name FROM calendars WHERE id = $1`, cal).Scan(&kept, &by, &name); err != nil {
		t.Fatalf("the calendar Sam unsubscribed from is gone, and with it that he read it: %v", err)
	}
	if !kept || by.UUID != samID || name != "Team" {
		t.Fatalf("the kept calendar: marked %v, by %s, named %q; want marked by Sam %s, named Team", kept, by.UUID, name, samID)
	}
	// The address's events are forgotten; what else was on it stays with it.
	var left []string
	rows, err := env.PG.Query(`SELECT id::text FROM events WHERE calendar_id = $1 ORDER BY id`, cal)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	rows.Close()
	if len(left) != 1 || left[0] != other.String() {
		t.Fatalf("events left on the kept calendar %v; want only %s (the address's %v forgotten)", left, other, brought)
	}
	// Ria's share is kept, at its level, marked by Sam.
	var (
		level   string
		stopped bool
		stopBy  uuid.NullUUID
	)
	if err := env.PG.QueryRow(`SELECT level, deleted_at IS NOT NULL, deleted_by FROM calendar_shares WHERE calendar_id = $1 AND person_id = $2`,
		cal, riaID).Scan(&level, &stopped, &stopBy); err != nil {
		t.Fatalf("Ria's share of the unsubscribed calendar is gone, and with it that she had it: %v", err)
	}
	if level != calendarModel.ShareDetails || !stopped || stopBy.UUID != samID {
		t.Fatalf("Ria's kept share: level %q, stopped %v, by %s; want details, stopped, by Sam", level, stopped, stopBy.UUID)
	}
	// No read sees it, and its ledger row is gone.
	if v := seen(cal); v != (views{}) {
		t.Fatalf("after unsubscribing, the reads see %+v; want none to see it", v)
	}
	// By id it answers as every deleted thing does (I9): none, deleted, and
	// what Sam had before, which the trash goes by.
	if d := decided(cal); d.Level != accessBusiness.LevelNone || !d.Deleted || d.Was != accessBusiness.LevelView {
		t.Fatalf("after unsubscribing, the rules give Sam %v (deleted %v, was %v); want none, deleted, was view", d.Level, d.Deleted, d.Was)
	}
	var ledgered int
	if err := env.PG.QueryRow(`SELECT count(*) FROM calendar_sync_calendars WHERE calendar_id = $1`, cal).Scan(&ledgered); err != nil || ledgered != 0 {
		t.Fatalf("the unsubscribed calendar has %d ledger rows (%v); want none", ledgered, err)
	}
	// Unsubscribing again finds nothing.
	if code := unsubscribe(sam, cal); code != http.StatusNotFound {
		t.Fatalf("Sam unsubscribes again: HTTP %d; want 404", code)
	}

	// The same address again makes a new calendar; the kept one stays as it was.
	again := subscribe()
	if v := seen(again); !v.subscriptions || !v.calendars || !v.byID || !v.due {
		t.Fatalf("subscribed again, the reads see %+v; want the new calendar seen", v)
	}
	var live, marked int
	if err := env.PG.QueryRow(`SELECT count(*) FILTER (WHERE deleted_at IS NULL), count(*) FILTER (WHERE deleted_at IS NOT NULL AND deleted_by = $2)
		FROM calendars WHERE owner_id = $1 AND kind = 'subscribed'`, samID, samID).Scan(&live, &marked); err != nil {
		t.Fatal(err)
	}
	if live != 1 || marked != 1 {
		t.Fatalf("subscribed again: %d live and %d kept subscribed calendars; want 1 and 1", live, marked)
	}
}

func TestCalendarsSoftDeleteMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		files, err := filepath.Glob("../../migrations/*_calendars_soft_delete." + dir + ".sql")
		if err != nil || len(files) != 1 {
			t.Fatalf("migration calendars_soft_delete's %s file: %v %v", dir, files, err)
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	env := integration.SetupEnv(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	marks := func() (deletedAt, deletedBy bool) {
		t.Helper()
		var cols []string
		rows, err := env.PG.Query(`SELECT column_name FROM information_schema.columns WHERE table_name = 'calendars' AND column_name IN ('deleted_at', 'deleted_by')`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, c)
		}
		for _, c := range cols {
			deletedAt, deletedBy = deletedAt || c == "deleted_at", deletedBy || c == "deleted_by"
		}
		return deletedAt, deletedBy
	}
	who := uuid.New()
	kept, live := uuid.New(), uuid.New()
	// One subscribed calendar unsubscribed (kept, marked) and one live.
	exec(`INSERT INTO calendars (id, kind, name, deleted_at, deleted_by) VALUES ($1, 'subscribed', 'Kept', NOW(), $3), ($2, 'subscribed', 'Live', NULL, NULL)`, kept, live, who)
	state := func() (keptMarked bool, liveLive bool) {
		t.Helper()
		var k, l sql.NullBool
		if err := env.PG.QueryRow(`SELECT bool_or(deleted_at IS NOT NULL) FILTER (WHERE id = $1), bool_or(deleted_at IS NULL) FILTER (WHERE id = $2) FROM calendars`, kept, live).Scan(&k, &l); err != nil {
			t.Fatal(err)
		}
		return k.Valid && k.Bool, l.Valid && l.Bool
	}
	if at, by := marks(); !at || !by {
		t.Fatalf("at head calendars has deleted_at %v, deleted_by %v; want both", at, by)
	}
	exec(down)
	if at, by := marks(); !at || by {
		t.Fatalf("after the down calendars has deleted_at %v, deleted_by %v; want deleted_at (345's) only", at, by)
	}
	if k, l := state(); !k || !l {
		t.Fatalf("after the down: the kept calendar still marked %v, the live one live %v; want both as they were", k, l)
	}
	exec(up)
	if at, by := marks(); !at || !by {
		t.Fatalf("after the up again calendars has deleted_at %v, deleted_by %v; want both", at, by)
	}
	exec(`UPDATE calendars SET deleted_by = $2 WHERE id = $1`, kept, who)
	var by uuid.NullUUID
	if err := env.PG.QueryRow(`SELECT deleted_by FROM calendars WHERE id = $1`, kept).Scan(&by); err != nil || by.UUID != who {
		t.Fatalf("after the up again, the kept calendar's mark: %v %v", by, err)
	}
	if k, l := state(); !k || !l {
		t.Fatalf("after the up again: the kept calendar marked %v, the live one live %v; want both as they were", k, l)
	}
}
