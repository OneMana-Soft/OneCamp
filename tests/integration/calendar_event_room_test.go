//go:build integration

package integration_test

// gcal-15 head B over HTTP: every OneCamp event has a call room without anyone
// adding one, on a workspace with guest access off. Nothing is made when the
// event is; the first join makes its room ready, the same room every time;
// whoever may read the event's details joins it (its organiser, its guests, a
// reader of its calendar above busy-only), and nobody else does: a busy-only
// reader, someone not on it, a details reader of a private event she isn't
// on, a deactivated member. Every guard a call passes after its token knows
// the room: its organiser runs it and records it, the recording files onto
// the event, a member in it stays in it on connect, reads its session, and
// its webhooks belong here. An event holding a call joins that call; a guest
// link added later goes on the room members are in; a copy a sync brought
// has none. The connect-time lookup of an event holding a room uses an index.
//
// Run: go test -tags=integration ./tests/integration/ -run '^(TestEveryEventHasACallRoomOverHTTP|TestEventsCallRoomIndexMigrationUpDownUp)$' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	_ "github.com/akashc777/OneCamp/business/CallRoom" // hands the calendar its event calls
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func TestEveryEventHasACallRoomOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-integration-tests")
	t.Setenv("JWT_SECRET", "event-room-integration-secret")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	integration.SetupMinio(t, "event-room-test")
	lk := integration.SetupLiveKit(t)
	egress := lk.UseFakeEgress(t)
	ctx := context.Background()
	guests := func(on bool) {
		t.Helper()
		if err := settingsBusiness.SetGuestAccessEnabled(on); err != nil {
			t.Fatal(err)
		}
		if err := settingsBusiness.SetGuestRoomsEnabled(on); err != nil {
			t.Fatal(err)
		}
	}
	guests(false) // a default workspace: members need no guest access
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	rae := seedCalendarPerson(t, env.PG, dg, "Rae")
	bo := seedCalendarPerson(t, env.PG, dg, "Bo")
	lee := seedCalendarPerson(t, env.PG, dg, "Lee")
	dex := seedCalendarPerson(t, env.PG, dg, "Dex")
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }
	made := func(title, extra string) calendarBusiness.Event {
		t.Helper()
		code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
			`{"title":"`+title+`","start":{"at":"`+at(9)+`","zone":"Europe/London"},"end":{"at":"`+at(10)+`"}`+extra+`}`, "")
		if code != http.StatusCreated {
			t.Fatalf("create %s: %d %v", title, code, body)
		}
		return dataEvent(t, body)
	}
	meetings := func(event string) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRowContext(ctx, `SELECT count(*) FROM call_meetings WHERE event_id = $1`, event).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	type entry struct {
		Token string `json:"token"`
		Room  string `json:"room"`
		Title string `json:"title"`
	}
	join := func(who *userModels.UserInfo, id string) (int, entry) {
		t.Helper()
		code, body := v2(t, eventController.JoinCalendarEventRoom, http.MethodPost, "/calendar/events/"+id+"/room/join", who, `{}`, id)
		var e entry
		_ = json.Unmarshal(body["data"], &e)
		return code, e
	}

	e := made("Weekly sync", `,"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"},{"user_uuid":"`+dex.UserDgraphInfo.Uuid+`"}]`)
	own := "meet-" + e.ID
	if n := meetings(e.ID); n != 0 {
		t.Fatalf("making the event made %d call rows, want none", n)
	}

	// Rae reads Sam's calendar with its details; Bo only sees when he's busy.
	for _, who := range []*userModels.UserInfo{rae, bo} {
		if _, err := calendarBusiness.Events.PersonCalendar(ctx, who, sam.UserDgraphInfo.Uuid); err != nil {
			t.Fatal(err)
		}
	}
	cal, err := calendarBusiness.Events.PersonCalendar(ctx, rae, sam.UserDgraphInfo.Uuid)
	if err != nil {
		t.Fatal(err)
	}
	if err := calendarBusiness.Events.Share(ctx, sam, cal.String(), rae.UserDgraphInfo.Uuid, models.ShareDetails); err != nil {
		t.Fatal(err)
	}

	for name, who := range map[string]*userModels.UserInfo{"its organiser": sam, "its guest": maya, "a reader of its details": rae} {
		code, in := join(who, e.ID)
		if code != http.StatusOK || in.Room != own || in.Token == "" || in.Title != "Weekly sync" {
			t.Fatalf("%s joins: %d %+v; want a token for %s", name, code, in, own)
		}
	}
	if code, again := join(maya, e.ID); code != http.StatusOK || again.Room != own {
		t.Fatalf("Maya joins again: %d %+v", code, again)
	}
	if n := meetings(e.ID); n != 1 {
		t.Fatalf("its rooms after the joins: %d, want 1", n)
	}
	for name, who := range map[string]*userModels.UserInfo{"someone not on it": lee, "a busy-only reader": bo} {
		if code, in := join(who, e.ID); code != http.StatusNotFound || in.Token != "" {
			t.Errorf("%s joins: %d %+v, want 404", name, code, in)
		}
	}
	// Where to join, read before joining, makes nothing.
	if code, body := v2(t, eventController.GetCalendarEventRoom, http.MethodGet, "/calendar/events/"+e.ID+"/room", maya, "", e.ID); code != http.StatusOK ||
		!strings.Contains(string(body["data"]), `"Weekly sync"`) || strings.Contains(string(body["data"]), "token") {
		t.Errorf("Maya reads its room: %d %s", code, body["data"])
	}
	if code, _ := v2(t, eventController.GetCalendarEventRoom, http.MethodGet, "/calendar/events/"+e.ID+"/room", lee, "", e.ID); code != http.StatusNotFound {
		t.Errorf("Lee reads its room: %d, want 404", code)
	}

	// A private event: Rae, who reads Sam's details but may not change his
	// calendar, isn't let in; Sam is.
	private := made("Doctor", `,"visibility":"private"`)
	if code, _ := join(rae, private.ID); code != http.StatusNotFound {
		t.Errorf("Rae joins Sam's private event: %d, want 404", code)
	}
	if code, in := join(sam, private.ID); code != http.StatusOK || in.Room != "meet-"+private.ID {
		t.Errorf("Sam joins his private event: %d %+v", code, in)
	}

	// The guards after the token: its organiser runs the room though it has
	// no link; a member in it stays in it on connect and reads its session;
	// its webhooks are this workspace's.
	if host, err := liveKitBusiness.HostByRule(ctx, own, &sam.UserDgraphInfo, false); err != nil || !host {
		t.Errorf("Sam runs his event's room: %v, %v", host, err)
	}
	if host, _ := liveKitBusiness.HostByRule(ctx, own, &maya.UserDgraphInfo, false); host {
		t.Errorf("Maya runs Sam's event's room")
	}
	for name, c := range map[string]struct {
		who  *userModels.UserInfo
		want bool
	}{"its organiser": {sam, true}, "its guest": {maya, true}, "a reader of its details": {rae, true}, "someone not on it": {lee, false}, "a busy-only reader": {bo, false}} {
		if ok, err := liveKitBusiness.MemberMayBeInCall(ctx, own, c.who.UserDgraphInfo.Uid); err != nil || ok != c.want {
			t.Errorf("on connect, %s may be in it: %v, %v; want %v", name, ok, err, c.want)
		}
	}
	ref := callStateBusiness.Ref{Kind: callStateBusiness.KindMeeting, ID: e.ID}
	if ok, _, err := callStateBusiness.MayRead(ctx, maya, ref); err != nil || !ok {
		t.Errorf("Maya reads its call's session: %v, %v", ok, err)
	}
	if ok, _, _ := callStateBusiness.MayRead(ctx, lee, ref); ok {
		t.Errorf("Lee reads its call's session")
	}
	if !callStateBusiness.BelongsHere(ctx, own) {
		t.Errorf("its room's webhooks don't belong here")
	}

	// Sam records it (the one start every call uses); the recording files
	// onto the event. Maya, its guest, doesn't start it.
	record := func(who *userModels.UserInfo, on bool) int {
		body := `{"on":false}`
		if on {
			body = `{"on":true}`
		}
		return call(t, callsController.SetMeetingRecording, http.MethodPost, "/calls/meetings/"+e.ID+"/recording", who, body,
			map[string]string{"meeting_id": e.ID}).Code
	}
	if code := record(maya, true); code != http.StatusNotFound {
		t.Errorf("Maya starts its recording: %d, want 404", code)
	}
	if code := record(sam, true); code != http.StatusOK {
		t.Errorf("Sam starts its recording: %d, want 200", code)
	} else if started := egress.Started(own); len(started) != 1 {
		t.Errorf("recordings of its room: %d, want 1", len(started))
	} else {
		var node struct {
			Q []struct {
				Event string `json:"recording_event"`
			} `json:"q"`
		}
		if err := json.Unmarshal(dg.Query(t, `{ q(func: eq(recording_egress_id, "`+started[0].EgressId+`")) { recording_event } }`), &node); err != nil {
			t.Fatal(err)
		}
		if len(node.Q) != 1 || node.Q[0].Event != e.ID {
			t.Errorf("the recording's node = %+v, want one filed under the event %s", node.Q, e.ID)
		}
		if code := record(sam, false); code != http.StatusOK {
			t.Errorf("Sam stops its recording: %d", code)
		}
	}

	// Its read says it has a room and where a member joins it, no secret.
	code, body := v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+e.ID, maya, "", e.ID)
	var read struct {
		Room *struct {
			Join       string `json:"join"`
			GuestLinks bool   `json:"guest_links"`
		} `json:"room"`
	}
	if err := json.Unmarshal(body["data"], &read); code != http.StatusOK || err != nil || read.Room == nil || read.Room.Join != "/app/meet/e/"+e.ID ||
		read.Room.GuestLinks {
		t.Errorf("Maya's read of it: %d %+v, %v", code, read.Room, err)
	}

	// A guest link added later goes on the room members are in: one room.
	guests(true)
	if code, body := v2(t, eventController.AddCalendarEventCall, http.MethodPost, "/calendar/events/"+e.ID+"/call", sam, "", e.ID); code != http.StatusOK {
		t.Fatalf("Sam adds a guest link: %d %v", code, body)
	}
	var held string
	if err := env.PG.QueryRowContext(ctx, `SELECT call_room FROM events WHERE id = $1`, e.ID).Scan(&held); err != nil || held != own {
		t.Errorf("the guest link's room = %q, %v; want the room members are in, %s", held, err, own)
	}
	if n := meetings(e.ID); n != 1 {
		t.Errorf("its rooms with a guest link: %d, want 1", n)
	}
	guests(false)

	// An event holding a call made before joins that call; a later part of
	// its series sharing the call (a split's tail) is let in by the part it's
	// on, on connect too.
	older, tail := made("Retro", `,"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]`), made("Retro", `,"attendees":[{"user_uuid":"`+bo.UserDgraphInfo.Uuid+`"}]`)
	room := "meet-" + uuid.NewString()
	if _, err := env.PG.ExecContext(ctx, `INSERT INTO call_meetings (id, kind, owner, lobby, created_at, event_id) VALUES ($1, 'event', $2, 'invitees', now(), $3)`,
		strings.TrimPrefix(room, "meet-"), sam.UserDgraphInfo.Uuid, older.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.ExecContext(ctx, `UPDATE events SET call_room = $1 WHERE id = ANY($2::uuid[])`, room, "{"+older.ID+","+tail.ID+"}"); err != nil {
		t.Fatal(err)
	}
	if code, in := join(maya, older.ID); code != http.StatusOK || in.Room != room {
		t.Errorf("Maya joins an event holding a call: %d %+v, want %s", code, in, room)
	}
	if code, in := join(bo, tail.ID); code != http.StatusOK || in.Room != room {
		t.Errorf("Bo joins the tail sharing it: %d %+v, want %s", code, in, room)
	}
	for name, c := range map[string]struct {
		who  *userModels.UserInfo
		want bool
	}{"its guest": {maya, true}, "the tail's guest": {bo, true}, "someone on neither": {lee, false}} {
		if ok, err := liveKitBusiness.MemberMayBeInCall(ctx, room, c.who.UserDgraphInfo.Uid); err != nil || ok != c.want {
			t.Errorf("on connect to the shared call, %s may be in it: %v, %v; want %v", name, ok, err, c.want)
		}
	}

	// A copy a sync brought carries its own link: no OneCamp room.
	synced := made("From Google", "")
	if _, err := env.PG.ExecContext(ctx, `UPDATE events SET source = 'ics' WHERE id = $1`, synced.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := join(sam, synced.ID); code != http.StatusNotFound {
		t.Errorf("Sam joins a synced copy: %d, want 404", code)
	}
	if _, body := v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+synced.ID, sam, "", synced.ID); strings.Contains(string(body["data"]), `"room"`) {
		t.Errorf("a synced copy says it has a room: %s", body["data"])
	}

	// Dex, its guest, deactivated: the route's own auth refuses him, and on
	// connect he may not stay.
	joinRoute := customMiddleware.VerifyAuth(http.HandlerFunc(eventController.JoinCalendarEventRoom))
	asDex := func() int {
		t.Helper()
		token, err := helpers.SignSessionToken(dex.UserPostgresInfo.Id.String(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/calendar/events/"+e.ID+"/room/join", strings.NewReader(`{}`))
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("eventId", e.ID)
		joinRoute.ServeHTTP(rec, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))
		return rec.Code
	}
	if ok, _ := liveKitBusiness.MemberMayBeInCall(ctx, own, dex.UserDgraphInfo.Uid); !ok {
		t.Errorf("on connect, Dex (its guest) may not be in it before he's deactivated")
	}
	if code := asDex(); code == http.StatusUnauthorized {
		t.Fatalf("Dex, active, through the route's auth: %d", code)
	}
	if err := userBusiness.DeactivateUser(ctx, dex.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	if code := asDex(); code != http.StatusUnauthorized {
		t.Errorf("Dex, deactivated, joins: %d, want 401", code)
	}
	if ok, _ := liveKitBusiness.MemberMayBeInCall(ctx, own, dex.UserDgraphInfo.Uid); ok {
		t.Errorf("on connect, Dex, deactivated, may stay in it")
	}

	// The connect-time lookup of the events holding a room never scans them.
	tx, err := env.PG.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, `EXPLAIN SELECT id FROM events WHERE call_room = 'meet-x' AND call_room <> '' AND deleted_at IS NULL AND series_id IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	_ = rows.Close()
	if !strings.Contains(strings.Join(plan, "\n"), "events_call_room_idx") {
		t.Errorf("the events holding a room are found by a scan:\n%s", strings.Join(plan, "\n"))
	}
}

// Migration 587: the index on the events holding a call comes down and goes
// up again, on a database every migration has run on, keeping the events.
func TestEventsCallRoomIndexMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		files, err := filepath.Glob("../../migrations/*_events_call_room_index." + dir + ".sql")
		if err != nil || len(files) != 1 {
			t.Fatalf("its %s file: %v %v", dir, files, err)
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	if !strings.HasPrefix(up, "-- no-transaction\n") || !strings.Contains(up, "CONCURRENTLY") {
		t.Fatalf("its up isn't built without blocking writes:\n%s", up)
	}
	env := integration.SetupEnv(t)
	has := func() bool {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM pg_indexes WHERE tablename = 'events' AND indexname = 'events_call_room_idx'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
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
		if has() != step.want {
			t.Fatalf("step %d: the index is there = %v, want %v", i, !step.want, step.want)
		}
	}
}
