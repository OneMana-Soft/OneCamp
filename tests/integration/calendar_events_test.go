//go:build integration
// +build integration

package integration_test

// Calendar events against real Postgres and Dgraph: taking a guest off an
// event (CAL-BUG-1), clearing its notes (CAL-BUG-17), who may change it
// (CAL-BUG-16) and who may open it (CAL-BUG-7); and, with Redis, how Google
// Calendar is linked (CAL-BUG-8); and a gone Google copy forgotten without
// undoing a newer edit (audit C7).
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestTakingAGuestOff|TestClearedNotes|TestOnlyThePeopleOnAnEvent|TestGoogleCalendarLinks' -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	calendarAdapter "github.com/akashc777/OneCamp/adapter/Calendar"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	integrationBusiness "github.com/akashc777/OneCamp/business/Integration"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	integrationController "github.com/akashc777/OneCamp/controllers/Integration"
	calendarDomain "github.com/akashc777/OneCamp/domain/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/akashc777/OneCamp/tests/integration"
)

// seedCalendarPerson is someone in both stores, as the calendar reads them.
func seedCalendarPerson(t *testing.T, db *sql.DB, dg *integration.DgraphEnv, name string) *userModels.UserInfo {
	t.Helper()
	id := uuid.New()
	email := strings.ToLower(name) + "-" + id.String()[:8] + "@example.test"
	handle := strings.ToLower(name) + id.String()[:4]
	if _, err := db.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, email, handle, name); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	uids := dg.Mutate(t, map[string]any{"uid": "_:p", "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_email_id": email})
	return &userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: id, EmailID: email, Username: &handle},
		UserDgraphInfo:   dgraphStruct.DgraphUser{Uid: uids["p"], Uuid: id.String(), UserName: name, EmailID: email},
	}
}

// calendarStores are fresh Postgres and Dgraph for a calendar test, in the
// store the test runs with (integration.ForEachCalendarStore): the one store
// starts moved, a fresh database having nothing to move.
func calendarStores(t *testing.T) (*integration.Env, *integration.DgraphEnv) {
	t.Helper()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	if helpers.CalendarInPostgres() {
		integration.MarkCalendarMoved(t, env.PG)
	}
	return env, integration.SetupDgraph(t)
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// onCalendar reports whether an event is on someone's calendar between from
// and to, and whether they are busy then.
func onCalendar(t *testing.T, who *userModels.UserInfo, eventUUID string, from, to time.Time) (listed, busy bool) {
	t.Helper()
	ctx := context.Background()
	events, err := calendarBusiness.GetEventsList(ctx, who, &from, &to)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		listed = listed || e.Uuid == eventUUID
	}
	b, err := calendarBusiness.BusyFor(ctx, who.UserDgraphInfo.Uuid, from, to)
	if err != nil {
		t.Fatal(err)
	}
	return listed, len(b.Intervals) > 0
}

// CAL-BUG-1: a guest taken off an event loses it, from their calendar and
// their busy times, can't open it, and is told. It was a set mutation, which
// only ever adds to a list: they kept the event, title and notes included.
func TestTakingAGuestOffAnEventTakesItAway(t *testing.T) {
	integration.ForEachCalendarStore(t, func(t *testing.T) {
		ctx := context.Background()
		env, dg := calendarStores(t)
		sam := seedCalendarPerson(t, env.PG, dg, "Sam")
		maya := seedCalendarPerson(t, env.PG, dg, "Maya")
		jonas := seedCalendarPerson(t, env.PG, dg, "Jonas")

		start := time.Now().Add(48 * time.Hour).Truncate(time.Hour)
		end := start.Add(time.Hour)
		from, to := start.Add(-time.Hour), end.Add(time.Hour)
		input := calendarAdapter.CreateOrUpdateEventInput{Title: "Acquisition talks", Description: "Draft terms", StartTime: rfc(start), EndTime: rfc(end),
			Participants: []string{maya.UserDgraphInfo.Uuid, jonas.UserDgraphInfo.Uuid}}
		out, err := calendarBusiness.CreateEvent(ctx, sam, input)
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.MustParse(out.EventUuid)
		if listed, busy := onCalendar(t, jonas, out.EventUuid, from, to); !listed || !busy {
			t.Fatalf("before: Jonas listed %v busy %v; the seed is wrong", listed, busy)
		}

		// Sam takes Jonas off.
		input.Participants = []string{maya.UserDgraphInfo.Uuid}
		if err := calendarBusiness.UpdateEvent(ctx, id, input, sam); err != nil {
			t.Fatal(err)
		}
		if listed, busy := onCalendar(t, jonas, out.EventUuid, from, to); listed || busy {
			t.Fatalf("Jonas was taken off, yet the event is listed %v, busy %v", listed, busy)
		}
		if _, err := calendarBusiness.GetEvent(ctx, jonas, out.EventUuid); !errors.Is(err, calendarBusiness.ErrEventNotFound) {
			t.Fatalf("Jonas can still open it: %v", err)
		}
		if listed, busy := onCalendar(t, maya, out.EventUuid, from, to); !listed || !busy {
			t.Fatalf("Maya was kept, yet listed %v busy %v", listed, busy)
		}

		// Jonas is told, in his inbox, without the title; Maya and Sam aren't.
		// (In the one store each guest was also told they were asked: that
		// notice is calendar.invited, and is no news of Jonas going.)
		var kind, resource, actor, data string
		deadline := time.Now().Add(10 * time.Second)
		for {
			err := env.PG.QueryRow(`SELECT kind, resource_uuid, actor_uuid::text, data::text FROM resource_notices WHERE user_uuid = $1 AND kind <> 'calendar.invited'`,
				jonas.UserPostgresInfo.Id).Scan(&kind, &resource, &actor, &data)
			if err == nil {
				break
			}
			if !errors.Is(err, sql.ErrNoRows) || time.Now().After(deadline) {
				t.Fatalf("Jonas wasn't told: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if kind != "event.uninvited" || resource != out.EventUuid || actor != sam.UserDgraphInfo.Uuid || strings.Contains(data, "Acquisition") || !strings.Contains(data, rfc(start)) {
			t.Fatalf("Jonas's notice: %s %s by %s, %s", kind, resource, actor, data)
		}
		var others int
		if err := env.PG.QueryRow(`SELECT count(*) FROM resource_notices WHERE user_uuid IN ($1, $2) AND kind <> 'calendar.invited'`, maya.UserPostgresInfo.Id, sam.UserPostgresInfo.Id).Scan(&others); err != nil || others != 0 {
			t.Fatalf("%d notices for Maya and Sam (%v)", others, err)
		}

		// A save that doesn't name the guests leaves them as they are.
		input.Participants = nil
		input.Title = "Acquisition talks, moved"
		if err := calendarBusiness.UpdateEvent(ctx, id, input, sam); err != nil {
			t.Fatal(err)
		}
		if listed, _ := onCalendar(t, maya, out.EventUuid, from, to); !listed {
			t.Fatal("a save without a guest list took Maya off")
		}

		// An empty list takes everyone off.
		input.Participants = []string{}
		if err := calendarBusiness.UpdateEvent(ctx, id, input, sam); err != nil {
			t.Fatal(err)
		}
		if listed, busy := onCalendar(t, maya, out.EventUuid, from, to); listed || busy {
			t.Fatalf("an empty guest list left Maya on it: listed %v busy %v", listed, busy)
		}
		if listed, _ := onCalendar(t, sam, out.EventUuid, from, to); !listed {
			t.Fatal("the creator lost their own event")
		}
	})
}

// CAL-BUG-17: cleared notes stay cleared. An empty string never reached the
// store (omitempty), so the old notes came back after the save.
func TestClearedNotesStayCleared(t *testing.T) {
	integration.ForEachCalendarStore(t, func(t *testing.T) {
		ctx := context.Background()
		env, dg := calendarStores(t)
		sam := seedCalendarPerson(t, env.PG, dg, "Sam")
		start := time.Now().Add(24 * time.Hour).Truncate(time.Hour)
		input := calendarAdapter.CreateOrUpdateEventInput{Title: "Standup", Description: "Passcode 4417", StartTime: rfc(start), EndTime: rfc(start.Add(30 * time.Minute))}
		out, err := calendarBusiness.CreateEvent(ctx, sam, input)
		if err != nil {
			t.Fatal(err)
		}
		input.Description = ""
		if err := calendarBusiness.UpdateEvent(ctx, uuid.MustParse(out.EventUuid), input, sam); err != nil {
			t.Fatal(err)
		}
		ev, err := calendarBusiness.GetEvent(ctx, sam, out.EventUuid)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Description != "" {
			t.Fatalf("the notes came back: %q", ev.Description)
		}
		from, to := start.Add(-time.Hour), start.Add(time.Hour)
		list, err := calendarBusiness.GetEventsList(ctx, sam, &from, &to)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range list {
			if e.Uuid == out.EventUuid && e.Description != "" {
				t.Fatalf("the calendar shows the old notes: %q", e.Description)
			}
		}
		if ev.Title != "Standup" {
			t.Fatalf("the rest of the event changed: %+v", ev)
		}
	})
}

// Google says an event's copy is gone after a newer edit has landed: only the
// copy is forgotten, and the newer edit stays (audit C7). The old store's
// cleanup wrote the older edit's title, notes and times back over it.
func TestAGoneGoogleCopyIsForgottenWithoutUndoingANewerEdit(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "graph")
	ctx := context.Background()
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	start := time.Now().Add(24 * time.Hour).Truncate(time.Hour)
	in := calendarAdapter.CreateOrUpdateEventInput{Title: "Planning", StartTime: rfc(start), EndTime: rfc(start.Add(time.Hour))}
	out, err := calendarBusiness.CreateEvent(ctx, sam, in)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(out.EventUuid)
	// Its copy on Google, in both of the old store's places.
	if _, err := env.PG.Exec(`UPDATE calendar_events SET google_calendar_event_id = 'gone-1' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var found struct {
		E []struct{ UID string } `json:"e"`
	}
	if err := json.Unmarshal(dg.Query(t, `{ e(func: eq(event_uuid, "`+out.EventUuid+`")) { uid } }`), &found); err != nil || len(found.E) != 1 {
		t.Fatalf("the event in the graph: %+v, %v", found, err)
	}
	dg.Mutate(t, map[string]any{"uid": found.E[0].UID, "event_google_calendar_id": "gone-1"})

	// The first edit's write to Google answers "gone" only once the second
	// edit has landed.
	release, first := make(chan struct{}), make(chan struct{}, 1)
	first <- struct{}{}
	defer calendarBusiness.SwapGoogleEventUpdateForTest(func(_ context.Context, gid string, _ calendarAdapter.CreateOrUpdateEventInput, _ *userModels.UserInfo, _ bool) error {
		select {
		case <-first:
			<-release
			return &integrationBusiness.GCalEventGoneError{GoogleEventId: gid}
		default:
			return nil
		}
	})()
	in.Title = "Planning (first edit)"
	if err := calendarBusiness.UpdateEvent(ctx, id, in, sam); err != nil {
		t.Fatal(err)
	}
	in.Title, in.Description = "Planning (second edit)", "Agenda attached"
	if err := calendarBusiness.UpdateEvent(ctx, id, in, sam); err != nil {
		t.Fatal(err)
	}
	close(release)
	bookingEventually(t, "the gone copy forgotten", func() bool {
		var gone bool
		return env.PG.QueryRow(`SELECT google_calendar_event_id IS NULL FROM calendar_events WHERE id = $1`, id).Scan(&gone) == nil && gone
	})
	var title, notes string
	if err := env.PG.QueryRow(`SELECT title, description FROM calendar_events WHERE id = $1`, id).Scan(&title, &notes); err != nil {
		t.Fatal(err)
	}
	if title != "Planning (second edit)" || notes != "Agenda attached" {
		t.Errorf("forgetting the copy put back an older edit: %q, %q", title, notes)
	}
	bookingEventually(t, "the gone copy forgotten in the graph too", func() bool {
		return graphGoogleCopy(t, dg, found.E[0].UID) == ""
	})
}

// Google says a copy is gone after a newer one was made: the graph forgets
// only the gone one, as the row does (audit C7).
func TestTheGraphForgetsAGoogleCopyOnlyWhileItNamesTheGoneOne(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "graph")
	ctx := context.Background()
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	start := time.Now().Add(24 * time.Hour).Truncate(time.Hour)
	out, err := calendarBusiness.CreateEvent(ctx, sam, calendarAdapter.CreateOrUpdateEventInput{Title: "Planning", StartTime: rfc(start), EndTime: rfc(start.Add(time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	var found struct {
		E []struct{ UID string } `json:"e"`
	}
	if err := json.Unmarshal(dg.Query(t, `{ e(func: eq(event_uuid, "`+out.EventUuid+`")) { uid } }`), &found); err != nil || len(found.E) != 1 {
		t.Fatalf("the event in the graph: %+v, %v", found, err)
	}
	uid := found.E[0].UID
	dg.Mutate(t, map[string]any{"uid": uid, "event_google_calendar_id": "copy-2"})

	if err := calendarDomain.ForgetDgraphEventGoogleCopy(ctx, uid, "gone-1"); err != nil {
		t.Fatal(err)
	}
	if got := graphGoogleCopy(t, dg, uid); got != "copy-2" {
		t.Errorf("forgetting the gone copy took the newer one: the graph names %q", got)
	}
	if err := calendarDomain.ForgetDgraphEventGoogleCopy(ctx, uid, "copy-2"); err != nil {
		t.Fatal(err)
	}
	if got := graphGoogleCopy(t, dg, uid); got != "" {
		t.Errorf("forgetting the copy it names left %q", got)
	}
}

// Item 11 (V-78, the owner: every delete soft): with CALENDAR_STORE=graph an
// event's delete kept its row (calendar_events.deleted_at) but took its graph
// node away. The node stays now, marked deleted, and every read of the old
// stores leaves it out: its maker's and its guest's calendars and busy times,
// opening it, who may open it, and changing, leaving or deleting it.
func TestADeletedEventKeepsItsGraphNodeOutOfEveryRead(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "graph")
	ctx := context.Background()
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	start := time.Now().Add(48 * time.Hour).Truncate(time.Hour)
	end := start.Add(time.Hour)
	from, to := start.Add(-time.Hour), end.Add(time.Hour)
	in := calendarAdapter.CreateOrUpdateEventInput{Title: "Offsite", StartTime: rfc(start), EndTime: rfc(end), Participants: []string{maya.UserDgraphInfo.Uuid}}
	out, err := calendarBusiness.CreateEvent(ctx, sam, in)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(out.EventUuid)
	res := accessBusiness.Resource{Kind: calendarBusiness.KindEvent, UUID: out.EventUuid}
	mayOpen := func(who *userModels.UserInfo) bool {
		d, err := accessBusiness.ResolveAll(ctx, accessBusiness.Subject{UserUID: who.UserDgraphInfo.Uid, UserUUID: who.UserDgraphInfo.Uuid}, []accessBusiness.Resource{res})
		if err != nil {
			t.Fatal(err)
		}
		return d[res].Level > accessBusiness.LevelNone
	}
	people := []*userModels.UserInfo{sam, maya}
	// Over the routes too: the calendar's list, opening it, and its delete.
	listedOver := func(who *userModels.UserInfo) bool {
		q := url.Values{"startDate": {rfc(from)}, "endDate": {rfc(to)}}
		rec := call(t, eventController.GetEventsListController, http.MethodGet, "/event/getEvents?"+q.Encode(), who, "", nil)
		var body struct {
			Data []dgraphStruct.DgraphEvent `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != http.StatusOK || err != nil {
			t.Fatalf("%s's calendar over the route: %d %q (%v)", who.UserDgraphInfo.UserName, rec.Code, msgOf(rec), err)
		}
		for _, e := range body.Data {
			if e.Uuid == out.EventUuid {
				return true
			}
		}
		return false
	}
	open := func(who *userModels.UserInfo) *httptest.ResponseRecorder {
		return call(t, eventController.GetEventController, http.MethodGet, "/event/getEvent/"+out.EventUuid, who, "", map[string]string{"eventId": out.EventUuid})
	}
	del := func() *httptest.ResponseRecorder {
		return call(t, eventController.DeleteEventController, http.MethodDelete, "/event/deleteEvent/"+out.EventUuid, sam, "", map[string]string{"eventId": out.EventUuid})
	}
	for _, who := range people {
		if listed, busy := onCalendar(t, who, out.EventUuid, from, to); !listed || !busy || !mayOpen(who) || !listedOver(who) || open(who).Code != http.StatusOK {
			t.Fatalf("before: %s's calendar lists it %v, busy %v; the seed is wrong", who.UserDgraphInfo.UserName, listed, busy)
		}
	}

	if rec := del(); rec.Code != http.StatusOK {
		t.Fatalf("its maker's delete: %d %q", rec.Code, msgOf(rec))
	}
	var node struct {
		E []struct {
			Deleted time.Time `json:"event_deleted_at"`
		} `json:"e"`
	}
	if err := json.Unmarshal(dg.Query(t, `{ e(func: eq(event_uuid, "`+out.EventUuid+`")) { event_deleted_at } }`), &node); err != nil {
		t.Fatal(err)
	}
	if len(node.E) != 1 {
		t.Fatal("its graph node is gone: the delete is hard")
	}
	if !node.E[0].Deleted.After(time.Unix(0, 0)) {
		t.Fatalf("its graph node isn't marked deleted: %v", node.E[0].Deleted)
	}
	for _, who := range people {
		name := who.UserDgraphInfo.UserName
		if listed, busy := onCalendar(t, who, out.EventUuid, from, to); listed || busy {
			t.Errorf("deleted, yet %s's calendar lists it %v, busy %v", name, listed, busy)
		}
		if _, err := calendarBusiness.GetEvent(ctx, who, out.EventUuid); !errors.Is(err, calendarBusiness.ErrEventNotFound) {
			t.Errorf("deleted, yet %s opens it: %v", name, err)
		}
		if mayOpen(who) {
			t.Errorf("deleted, yet %s may open it", name)
		}
		if listedOver(who) {
			t.Errorf("deleted, yet %s's calendar lists it over the route", name)
		}
		if rec := open(who); rec.Code != http.StatusNotFound || msgOf(rec) != "This event isn't available." {
			t.Errorf("deleted, yet %s opening it hears %d %q", name, rec.Code, msgOf(rec))
		}
	}
	if err := calendarBusiness.UpdateEvent(ctx, id, in, sam); !errors.Is(err, calendarBusiness.ErrEventNotFound) {
		t.Errorf("deleted, yet changed: %v", err)
	}
	if err := calendarBusiness.LeaveEvent(ctx, id, maya); !errors.Is(err, calendarBusiness.ErrEventNotFound) {
		t.Errorf("deleted, yet left: %v", err)
	}
	if rec := del(); rec.Code != http.StatusNotFound {
		t.Errorf("deleted again: %d %q", rec.Code, msgOf(rec))
	}
}

// graphGoogleCopy is the Google copy an event's graph node names, or "".
func graphGoogleCopy(t *testing.T, dg *integration.DgraphEnv, uid string) string {
	t.Helper()
	var got struct {
		E []struct {
			ID string `json:"event_google_calendar_id"`
		} `json:"e"`
	}
	if err := json.Unmarshal(dg.Query(t, `{ e(func: uid(`+uid+`)) { event_google_calendar_id } }`), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.E) == 0 {
		return ""
	}
	return got.E[0].ID
}

// call runs a handler as someone, with chi's URL parameters.
func call(t *testing.T, h http.HandlerFunc, method, path string, who *userModels.UserInfo, body string, params map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, helpers.UserInfoContextKey, *who)
	rec := httptest.NewRecorder()
	h(rec, req.WithContext(ctx))
	return rec
}

func msgOf(rec *httptest.ResponseRecorder) string {
	var body struct {
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Msg
}

// CAL-BUG-7 and 16: an event opens for its creator and its guests, by its id,
// in any month; to anyone else it isn't there. Changing it is its creator's: a
// guest hears 403 in words, not 500 "Internal Error".
func TestOnlyThePeopleOnAnEventSeeItAndOnlyItsMakerChangesIt(t *testing.T) {
	integration.ForEachCalendarStore(t, func(t *testing.T) {
		ctx := context.Background()
		env, dg := calendarStores(t)
		sam := seedCalendarPerson(t, env.PG, dg, "Sam")
		maya := seedCalendarPerson(t, env.PG, dg, "Maya")
		jonas := seedCalendarPerson(t, env.PG, dg, "Jonas")
		start := time.Now().AddDate(0, 2, 0).Truncate(time.Hour) // two months out
		out, err := calendarBusiness.CreateEvent(ctx, sam, calendarAdapter.CreateOrUpdateEventInput{Title: "Board review", StartTime: rfc(start), EndTime: rfc(start.Add(time.Hour)),
			Participants: []string{maya.UserDgraphInfo.Uuid}})
		if err != nil {
			t.Fatal(err)
		}
		id := out.EventUuid
		unknown := uuid.NewString()

		get := func(who *userModels.UserInfo, eventID string) *httptest.ResponseRecorder {
			return call(t, eventController.GetEventController, http.MethodGet, "/event/getEvent/"+url.PathEscape(eventID), who, "", map[string]string{"eventId": url.PathEscape(eventID)})
		}
		for _, c := range []struct {
			who  *userModels.UserInfo
			id   string
			code int
		}{{sam, id, 200}, {maya, id, 200}, {jonas, id, 404}, {sam, unknown, 404}, {sam, "not-an-id", 400}} {
			rec := get(c.who, c.id)
			if rec.Code != c.code {
				t.Errorf("%s opening %s: %d %s, want %d", c.who.UserDgraphInfo.UserName, c.id, rec.Code, rec.Body.String(), c.code)
				continue
			}
			if c.code == 200 {
				var body struct {
					Data dgraphStruct.DgraphEvent `json:"data"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Data.Uuid != id || body.Data.Title != "Board review" || len(body.Data.Participants) != 1 {
					t.Errorf("%s got %s (%v)", c.who.UserDgraphInfo.UserName, rec.Body.String(), err)
				}
			}
			if c.code == 404 && msgOf(rec) != "This event isn't available." {
				t.Errorf("404 says %q", msgOf(rec))
			}
		}

		update := func(who *userModels.UserInfo, eventID string) *httptest.ResponseRecorder {
			body := `{"title":"Mine now","startTime":"` + rfc(start) + `","endTime":"` + rfc(start.Add(time.Hour)) + `"}`
			return call(t, eventController.UpdateEventController, http.MethodPost, "/event/updateEvent/"+eventID, who, body, map[string]string{"eventId": eventID})
		}
		if rec := update(maya, id); rec.Code != http.StatusForbidden || msgOf(rec) != "Only the person who made this event can change it." {
			t.Errorf("a guest editing: %d %q", rec.Code, msgOf(rec))
		}
		if rec := update(jonas, id); rec.Code != http.StatusNotFound {
			t.Errorf("a stranger editing: %d %q", rec.Code, msgOf(rec))
		}
		if rec := update(sam, unknown); rec.Code != http.StatusNotFound {
			t.Errorf("editing no event: %d %q", rec.Code, msgOf(rec))
		}
		del := func(who *userModels.UserInfo) int {
			return call(t, eventController.DeleteEventController, http.MethodDelete, "/event/deleteEvent/"+id, who, "", map[string]string{"eventId": id}).Code
		}
		leave := func(who *userModels.UserInfo) int {
			return call(t, eventController.LeaveEventController, http.MethodPost, "/event/leaveEvent/"+id, who, "", map[string]string{"eventId": id}).Code
		}
		if c := del(maya); c != http.StatusForbidden {
			t.Errorf("a guest deleting: %d", c)
		}
		if c := del(jonas); c != http.StatusNotFound {
			t.Errorf("a stranger deleting: %d", c)
		}
		if c := leave(sam); c != http.StatusForbidden {
			t.Errorf("its creator leaving: %d", c)
		}
		if c := leave(jonas); c != http.StatusNotFound {
			t.Errorf("a stranger leaving: %d", c)
		}
		ev, err := calendarBusiness.GetEvent(ctx, sam, id)
		if err != nil || ev.Title != "Board review" {
			t.Fatalf("the refused calls changed the event: %+v (%v)", ev, err)
		}
	})
}

// CAL-BUG-8: Google Calendar links through the connectors flow. Its state is
// one-shot, issued to the person who started linking and bound to Google
// Calendar; it was the person's own user id, so one link could tie a
// teammate's account to the sender's Google.
func TestGoogleCalendarLinksThroughAOneShotStateOfItsOwn(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	t.Setenv("GOOGLE_CLIENT_ID", "client-id.apps.googleusercontent.com")
	t.Setenv("GOOGLE_CLIENT_SECRET", "client-secret")
	t.Setenv("BACKEND_DOMAIN", "api.example.test")
	maya := uuid.NewString()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, display_name) VALUES ($1, $2, 'Maya')`, maya, maya[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	mayaInfo := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: uuid.MustParse(maya)}, UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: maya}}

	rec := call(t, integrationController.GetGoogleCalendarAuthUrl, http.MethodGet, "/integration/google-calendar/auth-url", mayaInfo, "", nil)
	var body struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
		t.Fatalf("auth url: %d %s", rec.Code, rec.Body.String())
	}
	link, err := url.Parse(body.Data.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := link.Query()
	state := q.Get("state")
	if state == "" || state == maya || strings.Contains(state, maya) {
		t.Fatalf("the state %q is, or carries, Maya's id", state)
	}
	if q.Get("redirect_uri") != "https://api.example.test/connector/oauth/callback" || !strings.Contains(q.Get("scope"), "calendar") {
		t.Fatalf("not the connectors flow: %s", body.Data.URL)
	}

	// The state is held for Maya and Google Calendar, ten minutes at most.
	bound, found, _ := redisStore.GetString(ctx, registry.ConnectorOAuthState, []string{state})
	if !found || bound != maya+":"+connectorBusiness.ProviderCalendar+":"+connectorBusiness.ReturnPageCalendar {
		t.Fatalf("state bound to %q", bound)
	}
	if ttl := redisInit.RedisClient.TTL(ctx, registry.ConnectorOAuthState.Build(state)).Val(); ttl <= 0 || ttl > 10*time.Minute {
		t.Fatalf("state lives %v", ttl)
	}

	// A state the server never issued links nobody.
	if _, err := connectorBusiness.HandleCallback(ctx, "a-state-of-my-own", "code"); err == nil || !strings.Contains(err.Error(), "invalid or expired state") {
		t.Fatalf("a forged state: %v", err)
	}

	// Whoever follows Maya's link, it can only link Maya's account, and only
	// once. (No client from here on, so the exchange never reaches Google.)
	t.Setenv("GOOGLE_CLIENT_ID", "")
	res, _ := connectorBusiness.HandleCallback(ctx, state, "code")
	if res.UserUUID.String() != maya || res.ProviderID != connectorBusiness.ProviderCalendar || res.ReturnPage != connectorBusiness.ReturnPageCalendar {
		t.Fatalf("the state linked %+v", res)
	}
	if _, err := connectorBusiness.HandleCallback(ctx, state, "code"); err == nil || !strings.Contains(err.Error(), "invalid or expired state") {
		t.Fatalf("a state used twice: %v", err)
	}
}
