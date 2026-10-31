//go:build integration
// +build integration

package integration_test

// The calendar's one store (A1) against real Postgres 12 and Dgraph: its
// migration up, down and up; its API over HTTP (sequences, the trash, who sees
// what); and the move from the two old stores, every kind of event, rerun,
// checked against what the old stores' reads said, and timed on 50,000.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestCalendarStore|TestTheCalendarsOwnAPI|TestEveryReadOfTheStore|TestTheMove|TestMovingFiftyThousand' -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	calendarAdapter "github.com/akashc777/OneCamp/adapter/Calendar"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	calendarDomain "github.com/akashc777/OneCamp/domain/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// calendarStoreMigration is the one store's migration in one direction,
// found by name: the integrator numbers it at merge.
func calendarStoreMigration(t *testing.T, dir string) (string, string) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_calendar_store."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one calendar_store %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(matches[0]), string(b)
}

// laterMigrationsOn are the migrations after the one named, in one direction
// and newest first, whose SQL builds on any of these tables, or on a table one
// of them made: a key that references one, a change to one, or an index,
// trigger or view on one. They come down before the one named can drop its
// tables. They are judged by their SQL, not their names: 524_event_ooo_rules
// keys to events without "_calendar_" in its name, and the store's down
// couldn't drop events.
func laterMigrationsOn(t *testing.T, after, dir string, tables ...string) []struct{ name, sql string } {
	t.Helper()
	version := func(file string) int {
		n, _ := strconv.Atoi(strings.SplitN(file, "_", 2)[0])
		return n
	}
	named := regexp.MustCompile(`(?i)\b(REFERENCES|ON|TABLE|FROM|JOIN)\s+(?:IF\s+(?:NOT\s+)?EXISTS\s+)?(?:ONLY\s+)?(?:"?public"?\.)?"?([a-z_][a-z0-9_]*)"?`)
	on := map[string]bool{}
	for _, table := range tables {
		on[strings.ToLower(table)] = true
	}
	_, here, _, _ := runtime.Caller(0)
	dirOf := filepath.Join(filepath.Dir(here), "..", "..", "migrations")
	ups, _ := filepath.Glob(filepath.Join(dirOf, "*.up.sql"))
	sort.Slice(ups, func(i, j int) bool { return version(filepath.Base(ups[i])) < version(filepath.Base(ups[j])) })
	var out []struct{ name, sql string }
	for _, f := range ups {
		if version(filepath.Base(f)) <= version(after) {
			continue
		}
		up, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		uses := named.FindAllStringSubmatch(sqlCode(string(up)), -1)
		if !slices.ContainsFunc(uses, func(m []string) bool { return on[strings.ToLower(m[2])] }) {
			continue
		}
		// What it makes is built on in turn.
		for _, m := range uses {
			if strings.EqualFold(m[1], "TABLE") {
				on[strings.ToLower(m[2])] = true
			}
		}
		name := strings.TrimSuffix(filepath.Base(f), "up.sql") + dir + ".sql"
		b, err := os.ReadFile(filepath.Join(dirOf, name))
		if err != nil {
			t.Fatal(err)
		}
		out = append([]struct{ name, sql string }{{name, string(b)}}, out...)
	}
	return out
}

// The migration goes up, down and up again on Postgres 12, makes no ids of
// its own, and its rules hold: an event is timed or all-day, never both, and
// never ends before it starts; a guest is someone.
func TestCalendarStoreMigrationUpDownUp(t *testing.T) {
	name, up := calendarStoreMigration(t, "up")
	_, down := calendarStoreMigration(t, "down")
	makesNoIDs(t, name, up)
	env := integration.SetupEnv(t)
	tables := []string{"calendars", "events", "event_attendees"}
	// Later migrations build on these tables (A2's answers and guests by
	// email, A5's reminders, A4's imports, B6's sync ledger, A6's shares, the
	// out-of-office rules, their indexes, and whatever comes next): they come
	// down first, newest first, so this one can.
	for _, later := range laterMigrationsOn(t, name, "down", tables...) {
		if _, err := env.PG.Exec(later.sql); err != nil {
			t.Fatalf("%s: %v", later.name, err)
		}
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
		for _, table := range tables {
			if tableExists(t, env.PG, table) != step.want {
				t.Fatalf("step %d: %s exists = %v", i, table, !step.want)
			}
		}
	}
	// calendar_events is never touched by it.
	if !tableExists(t, env.PG, "calendar_events") {
		t.Fatal("the down migration took calendar_events with it")
	}

	user, calendar := uuid.New(), uuid.New()
	exec := func(q string, args ...any) error { _, err := env.PG.Exec(q, args...); return err }
	if err := exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'u', 'u', NOW(), NOW())`, user, user.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO calendars (id, owner_id) VALUES ($1, $2)`, calendar, user); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO calendars (id, owner_id) VALUES ($1, $2)`, uuid.New(), user); err == nil {
		t.Error("a second personal calendar")
	}
	insert := func(start, end, startDate, endDate any) error {
		id := uuid.New()
		return exec(`INSERT INTO events (id, calendar_id, created_by, ical_uid, start_at, end_at, start_date, end_date) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, calendar, user, id.String()+"@onecamp", start, end, startDate, endDate)
	}
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	if err := insert(now, now.Add(time.Hour), nil, nil); err != nil {
		t.Fatalf("a timed event: %v", err)
	}
	if err := insert(nil, nil, "2026-10-16", "2026-10-17"); err != nil {
		t.Fatalf("an all-day event: %v", err)
	}
	for name, args := range map[string][4]any{
		"both":            {now, now.Add(time.Hour), "2026-10-16", "2026-10-17"},
		"neither":         {nil, nil, nil, nil},
		"ending first":    {now, now.Add(-time.Minute), nil, nil},
		"no days":         {nil, nil, "2026-10-16", "2026-10-16"},
		"half an instant": {now, nil, nil, nil},
	} {
		if err := insert(args[0], args[1], args[2], args[3]); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	var event uuid.UUID
	if err := env.PG.QueryRow(`SELECT id FROM events LIMIT 1`).Scan(&event); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO event_attendees (event_id) VALUES ($1)`, event); err == nil {
		t.Error("a guest who is nobody")
	}
	if err := exec(`INSERT INTO event_attendees (event_id, user_id) VALUES ($1, $2)`, event, user); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO event_attendees (event_id, user_id) VALUES ($1, $2)`, event, user); err == nil {
		t.Error("a guest twice")
	}
}

// v2 is an /calendar/events call as someone, answered by its handler.
func v2(t *testing.T, h http.HandlerFunc, method, path string, who *userModels.UserInfo, body string, id string) (int, map[string]json.RawMessage) {
	t.Helper()
	params := map[string]string{}
	if id != "" {
		params["eventId"] = id
	}
	rec := call(t, h, method, path, who, body, params)
	var out map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func dataEvent(t *testing.T, body map[string]json.RawMessage) calendarBusiness.Event {
	t.Helper()
	var e calendarBusiness.Event
	if err := json.Unmarshal(body["data"], &e); err != nil {
		t.Fatalf("no event in %v: %v", body, err)
	}
	return e
}

// The calendar's own API over HTTP, with the one store on: made (201), read,
// listed, changed at its sequence (409 with the event as it is now when
// someone else changed it first), deleted to the trash and back. Off, it says
// so with a 503.
func TestTheCalendarsOwnAPIOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	jonas := seedCalendarPerson(t, env.PG, dg, "Jonas")

	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Design review","notes":"Bring the mocks","start":{"at":"2026-11-03T10:00:00Z","zone":"Europe/London"},"end":{"at":"2026-11-03T11:00:00Z"},`+
			`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	e := dataEvent(t, body)
	if e.Sequence != 0 || e.Start.Zone != "Europe/London" || len(e.Attendees) != 1 || e.Attendees[0].Name != "Maya" || e.Organiser.Name != "Sam" {
		t.Fatalf("made %+v", e)
	}

	for who, want := range map[*userModels.UserInfo]int{sam: 200, maya: 200, jonas: 404} {
		if code, body := v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+e.ID, who, "", e.ID); code != want {
			t.Errorf("%s reads it: %d %v", who.UserDgraphInfo.UserName, code, body)
		}
	}
	code, body = v2(t, eventController.ListCalendarEvents, http.MethodGet, "/calendar/events?from=2026-11-01T00:00:00Z&to=2026-11-08T00:00:00Z", maya, "", "")
	var list []calendarBusiness.Event
	if err := json.Unmarshal(body["data"], &list); code != 200 || err != nil || len(list) != 1 || list[0].ID != e.ID {
		t.Fatalf("Maya's week: %d %v %v", code, body, err)
	}
	if code, _ := v2(t, eventController.ListCalendarEvents, http.MethodGet, "/calendar/events?from=2026-01-01T00:00:00Z&to=2027-06-01T00:00:00Z", maya, "", ""); code != http.StatusBadRequest {
		t.Errorf("seventeen months at once: %d", code)
	}

	patch := func(who *userModels.UserInfo, body string) (int, map[string]json.RawMessage) {
		return v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+e.ID, who, body, e.ID)
	}
	if code, body := patch(sam, `{"sequence":0,"title":"Design review, round 2"}`); code != 200 || dataEvent(t, body).Sequence != 1 {
		t.Fatalf("Sam's change: %d %v", code, body)
	}
	code, body = patch(sam, `{"sequence":0,"title":"From an old tab"}`)
	if code != http.StatusConflict || dataEvent(t, body).Title != "Design review, round 2" {
		t.Fatalf("an old tab's change: %d %v", code, body)
	}
	if code, body := patch(maya, `{"sequence":1,"title":"Mine"}`); code != http.StatusForbidden {
		t.Errorf("a guest's change: %d %v", code, body)
	}
	if code, _ := patch(jonas, `{"sequence":1,"title":"Mine"}`); code != http.StatusNotFound {
		t.Errorf("a stranger's change: %d", code)
	}
	if code, _ := patch(sam, `{"sequence":1,"start":{"at":"2026-11-03T12:00:00Z"},"end":{"at":"2026-11-03T11:00:00Z"}}`); code != http.StatusBadRequest {
		t.Errorf("an end before its start: %d", code)
	}

	del := func(who *userModels.UserInfo, query string) int {
		code, _ := v2(t, eventController.DeleteCalendarEvent, http.MethodDelete, "/calendar/events/"+e.ID+query, who, "", e.ID)
		return code
	}
	if c := del(sam, "?sequence=0"); c != http.StatusConflict {
		t.Errorf("a stale delete: %d", c)
	}
	if c := del(maya, ""); c != http.StatusForbidden {
		t.Errorf("a guest's delete: %d", c)
	}
	if c := del(sam, "?sequence=1"); c != 200 {
		t.Fatalf("delete: %d", c)
	}
	if code, _ := v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+e.ID, maya, "", e.ID); code != http.StatusNotFound {
		t.Errorf("a deleted event, for Maya: %d", code)
	}
	code, body = v2(t, eventController.CalendarTrash, http.MethodGet, "/calendar/events/trash", sam, "", "")
	if err := json.Unmarshal(body["data"], &list); code != 200 || err != nil || len(list) != 1 || list[0].DeletedAt == nil {
		t.Fatalf("the trash: %d %v", code, body)
	}
	if code, _ := v2(t, eventController.RestoreCalendarEvent, http.MethodPost, "/calendar/events/"+e.ID+"/restore", maya, "", e.ID); code != http.StatusNotFound {
		t.Errorf("a guest restoring: %d", code)
	}
	if code, body := v2(t, eventController.RestoreCalendarEvent, http.MethodPost, "/calendar/events/"+e.ID+"/restore", sam, "", e.ID); code != 200 || dataEvent(t, body).DeletedAt != nil {
		t.Fatalf("restore: %d %v", code, body)
	}

	// The old stores' server: not on here.
	t.Setenv("CALENDAR_STORE", "graph")
	code, body = v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+e.ID, sam, "", e.ID)
	if code != http.StatusServiceUnavailable || !strings.Contains(string(body["msg"]), "switched on") {
		t.Errorf("with the store off: %d %v", code, body)
	}
}

// G-CAL-PRIV for every read A1 serves: the organiser and a guest see the
// event, by the old routes, the API, the list and the resolver; a guest taken
// off, a member not on it and a workspace admin not on it see nothing, as if
// there were no event; a deactivated organiser's event is still its guests';
// a trashed event is nobody's; busy times give times, never titles.
func TestEveryReadOfTheStoreKeepsAnEventToItsPeople(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	ctx := context.Background()
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	jonas := seedCalendarPerson(t, env.PG, dg, "Jonas")
	ana := seedCalendarPerson(t, env.PG, dg, "Ana")
	admin := seedCalendarPerson(t, env.PG, dg, "Admin")
	admin.UserPostgresInfo.IsAdmin = true
	dex := seedCalendarPerson(t, env.PG, dg, "Dex")

	start := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Hour)
	in := calendarAdapter.CreateOrUpdateEventInput{Title: "Acquisition talks", Description: "Draft terms",
		StartTime: rfc(start), EndTime: rfc(start.Add(time.Hour)), Participants: []string{maya.UserDgraphInfo.Uuid, ana.UserDgraphInfo.Uuid}}
	out, err := calendarBusiness.CreateEvent(ctx, sam, in)
	if err != nil {
		t.Fatal(err)
	}
	id := out.EventUuid
	in.Participants = []string{maya.UserDgraphInfo.Uuid} // Ana taken off
	if err := calendarBusiness.UpdateEvent(ctx, uuid.MustParse(id), in, sam); err != nil {
		t.Fatal(err)
	}
	dexEvent, err := calendarBusiness.CreateEvent(ctx, dex, calendarAdapter.CreateOrUpdateEventInput{Title: "Handover",
		StartTime: rfc(start), EndTime: rfc(start.Add(30 * time.Minute)), Participants: []string{maya.UserDgraphInfo.Uuid}})
	if err != nil {
		t.Fatal(err)
	}
	if err := userBusiness.DeactivateUser(ctx, dex.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	trashed, err := calendarBusiness.CreateEvent(ctx, sam, calendarAdapter.CreateOrUpdateEventInput{Title: "Cancelled offsite",
		StartTime: rfc(start), EndTime: rfc(start.Add(2 * time.Hour)), Participants: []string{maya.UserDgraphInfo.Uuid}})
	if err != nil {
		t.Fatal(err)
	}
	if err := calendarBusiness.DeleteEvent(ctx, uuid.MustParse(trashed.EventUuid), sam); err != nil {
		t.Fatal(err)
	}

	from, to := start.Add(-time.Hour), start.Add(3*time.Hour)
	sees := func(who *userModels.UserInfo, eventID string) (old, api, listed bool, level accessBusiness.Level) {
		_, err := calendarBusiness.GetEvent(ctx, who, eventID)
		old = err == nil
		if err != nil && !errors.Is(err, calendarBusiness.ErrEventNotFound) {
			t.Fatalf("%s reading %s: %v", who.UserDgraphInfo.UserName, eventID, err)
		}
		_, err = calendarBusiness.Events.Get(ctx, who, eventID)
		api = err == nil
		list, err := calendarBusiness.GetEventsList(ctx, who, &from, &to)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range list {
			listed = listed || e.Uuid == eventID
		}
		d, err := accessBusiness.Rules.Resolve(ctx, accessBusiness.Subject{UserUID: who.UserDgraphInfo.Uid, UserUUID: who.UserDgraphInfo.Uuid}, accessBusiness.Resource{Kind: calendarBusiness.KindEvent, UUID: eventID})
		if err != nil {
			t.Fatal(err)
		}
		return old, api, listed, d.Level
	}
	for _, c := range []struct {
		name  string
		who   *userModels.UserInfo
		event string
		see   bool
		level accessBusiness.Level
	}{
		{"the organiser", sam, id, true, accessBusiness.LevelFull},
		{"a guest", maya, id, true, accessBusiness.LevelView},
		{"a guest taken off", ana, id, false, accessBusiness.LevelNone},
		{"a member not on it", jonas, id, false, accessBusiness.LevelNone},
		{"an admin not on it", admin, id, false, accessBusiness.LevelNone},
		{"a guest of a deactivated organiser", maya, dexEvent.EventUuid, true, accessBusiness.LevelView},
		{"the organiser, of the trashed", sam, trashed.EventUuid, false, accessBusiness.LevelNone},
		{"a guest, of the trashed", maya, trashed.EventUuid, false, accessBusiness.LevelNone},
	} {
		old, api, listed, level := sees(c.who, c.event)
		if old != c.see || api != c.see || listed != c.see || level != c.level {
			t.Errorf("%s: the old route %v, the API %v, the list %v, level %s; want %v and %s", c.name, old, api, listed, level, c.see, c.level)
		}
	}
	// Busy: anyone in the workspace may ask, and is told times only.
	busy, err := calendarBusiness.BusyFor(ctx, maya.UserDgraphInfo.Uuid, from, to)
	if err != nil || len(busy.Intervals) == 0 {
		t.Fatalf("Maya's busy times: %+v %v", busy, err)
	}
	if b, _ := json.Marshal(busy); strings.Contains(string(b), "Acquisition") || strings.Contains(string(b), "Handover") {
		t.Errorf("busy times name an event: %s", b)
	}
}

// oldStoreEvent is a graph Event node and its calendar_events row, as the
// two old stores held one, written straight in (no service is involved).
type oldStoreEvent struct {
	id                 uuid.UUID
	by                 *userModels.UserInfo
	title, notes       string
	start, end         time.Time
	guests             []*userModels.UserInfo
	focus, away        bool
	inGraph, inPG      bool
	deletedInPG        bool
	googleID           string
	extraGuestNodeUIDs []string
}

func writeOldEvent(t *testing.T, db *sql.DB, dg *integration.DgraphEnv, e oldStoreEvent) {
	t.Helper()
	if e.inPG {
		var deleted any
		if e.deletedInPG {
			deleted = time.Now()
		}
		if _, err := db.Exec(`INSERT INTO calendar_events (id, title, description, start_time, end_time, created_by, google_calendar_event_id, is_focus, deleted_at)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9)`, e.id, e.title, e.notes, e.start, e.end, e.by.UserPostgresInfo.Id, e.googleID, e.focus, deleted); err != nil {
			t.Fatal(err)
		}
	}
	if e.inGraph {
		var guests []map[string]any
		for _, g := range e.guests {
			guests = append(guests, map[string]any{"uid": g.UserDgraphInfo.Uid})
		}
		for _, uid := range e.extraGuestNodeUIDs {
			guests = append(guests, map[string]any{"uid": uid})
		}
		node := map[string]any{"dgraph.type": "Event", "event_uuid": e.id.String(), "event_title": e.title, "event_description": e.notes,
			"event_start_time": rfc(e.start), "event_end_time": rfc(e.end), "event_created_by": map[string]any{"uid": e.by.UserDgraphInfo.Uid},
			"event_participants": guests, "event_is_focus": e.focus, "event_is_away": e.away}
		if e.googleID != "" {
			node["event_google_calendar_id"] = e.googleID
		}
		dg.Mutate(t, node)
	}
}

// listOf is someone's events between from and to as the old routes answer,
// reduced to what must match between the stores: id, title, notes, guests
// (but ignored, someone the move can't make a guest) and when (an all-day
// event by its dates).
func listOf(t *testing.T, who *userModels.UserInfo, from, to time.Time, zone *time.Location, ignored string) map[string]string {
	t.Helper()
	list, err := calendarBusiness.GetEventsList(context.Background(), who, &from, &to)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range list {
		var guests []string
		for _, p := range e.Participants {
			if p.Uuid != ignored {
				guests = append(guests, p.Uuid)
			}
		}
		sort.Strings(guests)
		when := e.StartTime.UTC().Format(time.RFC3339) + "/" + e.EndTime.UTC().Format(time.RFC3339)
		if e.AllDay {
			when = e.StartDate + "/" + e.EndDate
		} else if isMidnightIn(*e.StartTime, zone) && isMidnightIn(*e.EndTime, zone) {
			// The graph's all-day event: its instants, read as dates in its zone.
			when = e.StartTime.In(zone).Format("2006-01-02") + "/" + e.EndTime.In(zone).Format("2006-01-02")
		}
		focus, away := e.IsFocus != nil && *e.IsFocus, e.IsAway != nil && *e.IsAway
		out[e.Uuid] = strings.Join([]string{e.Title, e.Description, when, strings.Join(guests, ","), strconv.FormatBool(focus), strconv.FormatBool(away)}, " | ")
	}
	return out
}

func isMidnightIn(t time.Time, loc *time.Location) bool {
	l := t.In(loc)
	return l.Hour() == 0 && l.Minute() == 0 && l.Second() == 0
}

// The move brings every event of the old stores across once, as people saw
// it: the old routes then answer the same for everyone. A dry run writes
// nothing; a second run changes nothing; an event changed in the new store
// is never touched again; one changed or deleted in the old stores before the
// switch is brought up to date, or to the trash; and what only Postgres had
// goes to the trash, nothing lost.
func TestTheMoveBringsEveryEventAcrossOnce(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "graph")
	ctx := context.Background()
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	jonas := seedCalendarPerson(t, env.PG, dg, "Jonas")
	if _, err := env.PG.Exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token, quiet_hours_tz) VALUES ($1, $2, 'Asia/Kolkata')`,
		maya.UserPostgresInfo.Id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	// Someone the graph knows and Postgres doesn't: a guest left out, and an
	// organiser whose events can't move.
	ghostUUID := uuid.NewString()
	ghost := dg.Mutate(t, map[string]any{"uid": "_:g", "dgraph.type": "User", "user_uuid": ghostUUID, "user_name": "Ghost"})["g"]
	ghostInfo := &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: ghost}}

	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	base := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Hour)
	launch := time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, kolkata).AddDate(0, 0, 2)
	ids := map[string]uuid.UUID{}
	for name, e := range map[string]oldStoreEvent{
		"meeting":   {by: sam, title: "Board", notes: "Pack", start: base, end: base.Add(time.Hour), guests: []*userModels.UserInfo{maya, jonas}, inGraph: true, inPG: true, googleID: "g-board"},
		"focus":     {by: sam, title: "Focus time", start: base.Add(3 * time.Hour), end: base.Add(5 * time.Hour), focus: true, inGraph: true, inPG: true},
		"away":      {by: maya, title: "Leave", start: launch.AddDate(0, 0, 1), end: launch.AddDate(0, 0, 3), away: true, inGraph: true, inPG: true},
		"launch":    {by: maya, title: "Launch day", start: launch, end: launch.AddDate(0, 0, 1), guests: []*userModels.UserInfo{jonas}, inGraph: true, inPG: true, extraGuestNodeUIDs: []string{ghost}},
		"pg only":   {by: sam, title: "Never shown", start: base, end: base.Add(time.Hour), inPG: true},
		"deleted":   {by: sam, title: "Deleted long ago", start: base, end: base.Add(time.Hour), inPG: true, deletedInPG: true},
		"graph gid": {by: jonas, title: "Synced", start: base.Add(time.Hour), end: base.Add(2 * time.Hour), inGraph: true, inPG: true},
	} {
		e.id = uuid.New()
		ids[name] = e.id
		writeOldEvent(t, env.PG, dg, e)
	}
	// The Google id only Postgres kept (the graph lost it, as LeaveEvent
	// feared), and a partial node beside the board meeting's.
	if _, err := env.PG.Exec(`UPDATE calendar_events SET google_calendar_event_id = 'g-synced' WHERE id = $1`, ids["graph gid"]); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, map[string]any{"dgraph.type": "Event", "event_uuid": ids["meeting"].String(), "event_google_calendar_id": "g-board"})
	ghostEvent := uuid.New()
	dg.Mutate(t, map[string]any{"dgraph.type": "Event", "event_uuid": ghostEvent.String(), "event_title": "Ghost's", "event_start_time": rfc(base),
		"event_end_time": rfc(base.Add(time.Hour)), "event_created_by": map[string]any{"uid": ghostInfo.UserDgraphInfo.Uid}})

	from, to := base.Add(-24*time.Hour), base.AddDate(0, 0, 7)
	people := map[string]*userModels.UserInfo{"Sam": sam, "Maya": maya, "Jonas": jonas}
	before := map[string]map[string]string{}
	for name, who := range people {
		before[name] = listOf(t, who, from, to, kolkata, ghostUUID)
	}
	count := func() int {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM events`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A dry run writes nothing.
	dry, err := calendarBusiness.MoveToPostgres(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Moved != 7 || dry.NoOrganiser != 1 || dry.PartialNodes != 1 || count() != 0 {
		t.Fatalf("the dry run: %s (%d rows)", dry, count())
	}

	r, err := calendarBusiness.MoveToPostgres(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Moved != 7 || r.Trashed != 2 || r.AllDay != 2 || r.Guests != 3 || r.GuestsLeft != 1 || r.NoOrganiser != 1 ||
		r.Different != 0 || r.Checked != 7 || r.Zones["quiet_hours"] != 2 {
		t.Fatalf("the move: %s", r)
	}

	// The switch: the old routes answer the same for everyone.
	t.Setenv("CALENDAR_STORE", "postgres")
	integration.MarkCalendarMoved(t, env.PG)
	for name, who := range people {
		after := listOf(t, who, from, to, kolkata, ghostUUID)
		for id, was := range before[name] {
			if after[id] != was {
				t.Errorf("%s's %s:\n before %s\n after  %s", name, id, was, after[id])
			}
		}
		if len(after) != len(before[name]) {
			t.Errorf("%s had %d events and has %d: %v", name, len(before[name]), len(after), after)
		}
	}
	got, err := calendarBusiness.Events.Get(ctx, sam, ids["meeting"].String())
	if err != nil || got.GoogleEventID != "g-board" || got.Attendees[0].Response != "accepted" {
		t.Fatalf("the board meeting: %+v %v", got, err)
	}
	if s, _ := calendarBusiness.Events.Get(ctx, jonas, ids["graph gid"].String()); s == nil || s.GoogleEventID != "g-synced" {
		t.Errorf("the Google id Postgres kept: %+v", s)
	}
	trash, _ := calendarBusiness.Events.Trash(ctx, sam, nil)
	inTrash := map[string]bool{}
	for _, e := range trash {
		inTrash[e.ID] = true
	}
	if !inTrash[ids["pg only"].String()] || !inTrash[ids["deleted"].String()] {
		t.Errorf("the trash holds %v", inTrash)
	}

	// Again: nothing changes.
	again, err := calendarBusiness.MoveToPostgres(ctx, true)
	if err != nil || again.Moved != 0 || again.Unchanged != 7 || again.Refreshed != 0 || again.Different != 0 {
		t.Fatalf("again: %s %v", again, err)
	}

	// Changed in the new store: kept. Changed in the old stores before the
	// switch (as on a server still running the graph): brought up to date.
	if err := calendarBusiness.UpdateEvent(ctx, ids["focus"], calendarAdapter.CreateOrUpdateEventInput{Title: "Focus, changed in the store",
		StartTime: rfc(base.Add(3 * time.Hour)), EndTime: rfc(base.Add(4 * time.Hour))}, sam); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CALENDAR_STORE", "graph")
	if err := calendarBusiness.UpdateEvent(ctx, ids["meeting"], calendarAdapter.CreateOrUpdateEventInput{Title: "Board, moved",
		Description: "Pack", StartTime: rfc(base.Add(time.Hour)), EndTime: rfc(base.Add(2 * time.Hour)), Participants: []string{maya.UserDgraphInfo.Uuid}}, sam); err != nil {
		t.Fatal(err)
	}
	if err := calendarBusiness.DeleteEvent(ctx, ids["graph gid"], jonas); err != nil {
		t.Fatal(err)
	}
	// And one gone from both old stores altogether.
	if _, err := env.PG.Exec(`DELETE FROM calendar_events WHERE id = $1`, ids["away"]); err != nil {
		t.Fatal(err)
	}
	if err := calendarDomain.DeleteDgraphEvent(ctx, ids["away"].String(), time.Now()); err != nil {
		t.Fatal(err)
	}
	// The deleted one comes back through its row, deleted: to the trash, as
	// does the one gone from both.
	third, err := calendarBusiness.MoveToPostgres(ctx, true)
	if err != nil || third.Kept != 1 || third.Refreshed != 2 || third.Withdrawn != 1 || third.Different != 0 {
		t.Fatalf("after changes on both sides: %s %v", third, err)
	}
	t.Setenv("CALENDAR_STORE", "postgres")
	if e, _ := calendarBusiness.Events.Get(ctx, sam, ids["meeting"].String()); e == nil || e.Title != "Board, moved" || len(e.Attendees) != 1 {
		t.Errorf("the board meeting, brought up to date: %+v", e)
	}
	if e, _ := calendarBusiness.Events.Get(ctx, sam, ids["focus"].String()); e == nil || e.Title != "Focus, changed in the store" {
		t.Errorf("the focus block, kept: %+v", e)
	}
	for _, gone := range []string{"graph gid", "away"} {
		e, err := calendarBusiness.Events.Get(ctx, maya, ids[gone].String())
		if gone == "graph gid" {
			e, err = calendarBusiness.Events.Get(ctx, jonas, ids[gone].String())
		}
		if !errors.Is(err, calendarBusiness.ErrEventNotFound) {
			t.Errorf("%s, gone from the old stores: %+v %v", gone, e, err)
		}
	}
}

// How long a big install waits for the move: 50,000 events, two guests each,
// over 25 organisers. It moves them all, reads every one back as meant, and a
// second run changes nothing; both runs' times are logged for the release
// notes. The work is counted rather than timed: one step per 500 events.
func TestMovingFiftyThousandEvents(t *testing.T) {
	const events, organisers, perMutation = 50000, 25, 2500
	t.Setenv("CALENDAR_STORE", "graph")
	ctx := context.Background()
	env, dg := calendarStores(t)
	var people []*userModels.UserInfo
	for i := 0; i < organisers; i++ {
		people = append(people, seedCalendarPerson(t, env.PG, dg, "P"+strconv.Itoa(i)))
	}
	start := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	ids := make([]uuid.UUID, 0, events)
	titles, starts, ends, creators := make([]string, 0, events), make([]time.Time, 0, events), make([]time.Time, 0, events), make([]string, 0, events)
	seeded := time.Now()
	for i := 0; i < events; i += perMutation {
		var nodes []map[string]any
		for j := i; j < i+perMutation && j < events; j++ {
			id := uuid.New()
			by := people[j%organisers]
			s := start.Add(time.Duration(j) * 30 * time.Minute)
			ids, titles, starts, ends, creators = append(ids, id), append(titles, "Event "+strconv.Itoa(j)), append(starts, s), append(ends, s.Add(30*time.Minute)), append(creators, by.UserPostgresInfo.Id.String())
			nodes = append(nodes, map[string]any{"dgraph.type": "Event", "event_uuid": id.String(), "event_title": "Event " + strconv.Itoa(j),
				"event_start_time": rfc(s), "event_end_time": rfc(s.Add(30 * time.Minute)), "event_created_by": map[string]any{"uid": by.UserDgraphInfo.Uid},
				"event_participants": []map[string]any{{"uid": people[(j+1)%organisers].UserDgraphInfo.Uid}, {"uid": people[(j+2)%organisers].UserDgraphInfo.Uid}}})
		}
		dg.Mutate(t, nodes)
	}
	if _, err := env.PG.Exec(`INSERT INTO calendar_events (id, title, start_time, end_time, created_by)
		SELECT * FROM unnest($1::uuid[], $2::text[], $3::timestamptz[], $4::timestamptz[], $5::uuid[])`,
		pq.Array(helpers.UUIDStrings(ids)), pq.Array(titles), pq.Array(starts), pq.Array(ends), pq.Array(creators)); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded %d events in the old stores in %v", events, time.Since(seeded).Round(time.Second))

	first, err := calendarBusiness.MoveToPostgres(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the move: %s", first)
	if first.Moved != events || first.Guests != 2*events || first.Checked != events || first.Different != 0 {
		t.Fatalf("moved %d of %d, %d guests, checked %d, %d different", first.Moved, events, first.Guests, first.Checked, first.Different)
	}
	if want := (events+499)/500 + 1; first.Pages > want {
		t.Errorf("%d steps for %d events, want at most %d", first.Pages, events, want)
	}
	second, err := calendarBusiness.MoveToPostgres(ctx, true)
	if err != nil || second.Unchanged != events || second.Moved != 0 || second.Refreshed != 0 {
		t.Fatalf("the second run: %s %v", second, err)
	}
	t.Logf("the second run: %s", second)
}
