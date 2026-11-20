//go:build integration

package integration_test

// Rooms (gcal-6 head 1): admins keep the directory; members list rooms, find
// free ones and book them on events they may edit; guests and bots get 403; a
// room refuses a double booking at write time, also from two writers at once;
// a room's bookings show only busy to anyone who may not see the event; Find a
// time counts a named room's bookings as busy. The wire shapes are decoded
// here, as the web will read them.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestCalendarRoomsMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		_, here, _, _ := runtime.Caller(0)
		b, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "601_calendar_rooms."+dir+".sql"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	env := integration.SetupEnv(t)
	made := func() (tables, indexes int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT
			(SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('calendar_rooms', 'calendar_room_bookings')),
			(SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname IN ('idx_calendar_rooms_live_name',
				'idx_fk_calendar_rooms_created_by', 'idx_fk_calendar_rooms_updated_by', 'idx_calendar_room_bookings_live',
				'idx_fk_calendar_room_bookings_room', 'idx_fk_calendar_room_bookings_event',
				'idx_fk_calendar_room_bookings_created_by', 'idx_fk_calendar_room_bookings_deleted_by'))`).Scan(&tables, &indexes); err != nil {
			t.Fatal(err)
		}
		return
	}
	for i, step := range []struct {
		name, sql string
		want      int
	}{{"up", up, 1}, {"down", down, 0}, {"up", up, 1}, {"down", down, 0}, {"up", up, 1}} {
		if _, err := env.PG.Exec(step.sql); err != nil {
			t.Fatalf("step %d (%s): %v", i, step.name, err)
		}
		if tables, indexes := made(); tables != 2*step.want || indexes != 8*step.want {
			t.Errorf("step %d (%s): %d tables, %d indexes; want %d, %d", i, step.name, tables, indexes, 2*step.want, 8*step.want)
		}
	}
	var cols int
	if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'calendar_rooms'
		AND column_name IN ('name', 'building', 'floor', 'capacity', 'features', 'deleted_at')`).Scan(&cols); err != nil || cols != 6 {
		t.Fatalf("calendar_rooms has %d of its 6 columns (%v)", cols, err)
	}
}

// roomRead is a room as the API sends it.
type roomRead struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Building string   `json:"building"`
	Floor    string   `json:"floor"`
	Capacity int      `json:"capacity"`
	Features []string `json:"features"`
	Retired  bool     `json:"retired"`
}

// roomBookingRead is one booking on a room's calendar.
type roomBookingRead struct {
	Start   time.Time  `json:"start"`
	End     time.Time  `json:"end"`
	Busy    bool       `json:"busy"`
	EventID string     `json:"event_id"`
	Title   string     `json:"title"`
	Rooms   []roomRead `json:"rooms"`
}

type roomsWorld struct {
	t      *testing.T
	routes http.Handler
}

func (w *roomsWorld) call(who uuid.UUID, method, path string, body any) (int, []byte) {
	w.t.Helper()
	token, err := helpers.SignSessionToken(who.String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
	if err != nil {
		w.t.Fatal(err)
	}
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
	req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
	rec := httptest.NewRecorder()
	w.routes.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// data decodes the body's "data" into v.
// patch changes an event as who: it reads the event's sequence first (as
// seenBy, when who may not see the event) and sends it, as a client does.
func (w *roomsWorld) patch(who uuid.UUID, id string, body map[string]any, seenBy ...uuid.UUID) (int, []byte) {
	w.t.Helper()
	reader := who
	if len(seenBy) > 0 {
		reader = seenBy[0]
	}
	code, raw := w.call(reader, "GET", "/calendar/events/"+id, nil)
	if code != http.StatusOK {
		w.t.Fatalf("read %s before changing it: %d %s", id, code, raw)
	}
	seen := data[struct {
		Sequence int `json:"sequence"`
	}](w.t, raw)
	send := map[string]any{"sequence": seen.Sequence}
	for k, v := range body {
		send[k] = v
	}
	return w.call(who, "PATCH", "/calendar/events/"+id, send)
}

func data[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var out struct {
		Data T `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out.Data
}

func roomsSetup(t *testing.T) (*roomsWorld, map[string]uuid.UUID) {
	t.Helper()
	t.Setenv("JWT_SECRET", "calendar-rooms-integration-secret")
	t.Setenv("CSRF_DISABLED", "1")
	t.Setenv("CALENDAR_STORE", "postgres")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	integration.SetupDgraph(t)
	db := postgresInit.DBConn.SqlDB
	integration.MarkCalendarMoved(t, db)
	people := map[string]uuid.UUID{}
	for _, name := range []string{"Ada", "Sam", "Maya", "Gus", "Bot"} {
		id, _, err := userBusiness.CreateUserWithMethod(ctx, name+"@rooms.test", name, nil, userModels.AuthMethodEmail, false)
		if err != nil {
			t.Fatal(err)
		}
		people[name] = id
	}
	for q, who := range map[string]uuid.UUID{
		// The admin gate reads admin_users (by email), as the server's own setup writes it.
		`INSERT INTO admin_users (email_id) SELECT email_id FROM users WHERE id = $1`: people["Ada"],
		`UPDATE users SET is_external = true WHERE id = $1`:                           people["Gus"],
		`UPDATE users SET is_bot = true WHERE id = $1`:                                people["Bot"],
	} {
		if _, err := db.Exec(q, who); err != nil {
			t.Fatal(err)
		}
	}
	return &roomsWorld{t: t, routes: router.Routes()}, people
}

func eventBody(title string, start time.Time, minutes int, rooms ...string) map[string]any {
	end := start.Add(time.Duration(minutes) * time.Minute)
	return map[string]any{"title": title, "start": map[string]any{"at": start, "zone": "UTC"},
		"end": map[string]any{"at": end, "zone": "UTC"}, "rooms": rooms}
}

func TestRoomsAreKeptByAdminsAndBookedByMembers(t *testing.T) {
	w, p := roomsSetup(t)
	ada, sam, maya, gus, bot := p["Ada"], p["Sam"], p["Maya"], p["Gus"], p["Bot"]
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 7)
	ten := day.Add(10 * time.Hour)

	// Admin CRUD; members and the rest are refused the admin gate.
	code, raw := w.call(ada, "POST", "/admin/calendar/rooms", map[string]any{"name": "Board room", "building": "HQ", "floor": "3", "capacity": 8, "features": []string{"screen"}})
	if code != http.StatusCreated {
		t.Fatalf("admin adds a room: %d %s", code, raw)
	}
	board := data[roomRead](t, raw)
	if board.ID == "" || board.Name != "Board room" || board.Capacity != 8 || board.Building != "HQ" || board.Floor != "3" || len(board.Features) != 1 {
		t.Fatalf("room as sent: %+v", board)
	}
	if code, _ := w.call(sam, "POST", "/admin/calendar/rooms", map[string]any{"name": "Sam's"}); code != http.StatusForbidden {
		t.Fatalf("a member adds a room: %d, want 403", code)
	}
	// A room sent without building, floor or features is still a room.
	code, raw = w.call(ada, "POST", "/admin/calendar/rooms", map[string]any{"name": "Huddle", "capacity": 3})
	huddle := data[roomRead](t, raw)
	if code != http.StatusCreated || huddle.ID == "" || huddle.Features == nil {
		t.Fatalf("admin adds a bare room: %d %s", code, raw)
	}
	code, raw = w.call(ada, "POST", "/admin/calendar/rooms", map[string]any{"name": "Old wing", "capacity": 20})
	old := data[roomRead](t, raw)
	if code != http.StatusCreated || old.ID == "" {
		t.Fatalf("admin adds a third room: %d %s", code, raw)
	}
	if code, raw := w.call(ada, "PATCH", "/admin/calendar/rooms/"+board.ID, map[string]any{"capacity": 10}); code != http.StatusOK || data[roomRead](t, raw).Capacity != 10 {
		t.Fatalf("admin edits a room: %d %s", code, raw)
	}
	if code, _ := w.call(ada, "PATCH", "/admin/calendar/rooms/"+uuid.NewString(), map[string]any{"capacity": 1}); code != http.StatusNotFound {
		t.Fatalf("an unknown room: %d, want 404", code)
	}

	// Members list rooms; guests and bots see no directory.
	code, raw = w.call(maya, "GET", "/calendar/rooms", nil)
	if listed := data[[]roomRead](t, raw); code != http.StatusOK || len(listed) != 3 {
		t.Fatalf("a member lists rooms: %d %s", code, raw)
	}
	// A guest or a bot holds no session here: the session gate turns them away
	// (401) before the rooms code; the service's own 403 for whoever reaches it
	// past the gate is TestOnlyMembersReachTheRoomDirectory.
	for name, who := range map[string]uuid.UUID{"guest": gus, "bot": bot} {
		if code, _ := w.call(who, "GET", "/calendar/rooms", nil); code != http.StatusUnauthorized {
			t.Errorf("a %s lists rooms: %d, want 401", name, code)
		}
		if code, _ := w.call(who, "POST", "/calendar/events", eventBody("x", ten, 30, board.ID)); code != http.StatusUnauthorized {
			t.Errorf("a %s books a room: %d, want 401", name, code)
		}
	}

	// A member books; the event says its rooms.
	code, raw = w.call(sam, "POST", "/calendar/events", eventBody("Budget review", ten, 60, board.ID))
	if code != http.StatusCreated {
		t.Fatalf("a member books a room: %d %s", code, raw)
	}
	booked := data[struct {
		ID    string     `json:"id"`
		Rooms []roomRead `json:"rooms"`
	}](t, raw)
	if len(booked.Rooms) != 1 || booked.Rooms[0].Name != "Board room" {
		t.Fatalf("the event's rooms: %+v", booked.Rooms)
	}

	// A double booking is refused with the room and the date, and names the
	// other event only to someone who may see it.
	code, raw = w.call(maya, "POST", "/calendar/events", eventBody("Clash", ten.Add(30*time.Minute), 60, board.ID))
	var clash struct {
		Code string `json:"code"`
		Data struct {
			RoomID  string `json:"room_id"`
			Room    string `json:"room"`
			Date    string `json:"date"`
			EventID string `json:"event_id"`
			Title   string `json:"title"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &clash)
	if code != http.StatusConflict || clash.Code != "room_taken" || clash.Data.Room != "Board room" || clash.Data.Date != ten.Format("2006-01-02") {
		t.Fatalf("a double booking: %d %s", code, raw)
	}
	if clash.Data.EventID != "" || clash.Data.Title != "" {
		t.Errorf("the refusal names an event Maya may not see: %s", raw)
	}
	if code, raw := w.call(sam, "POST", "/calendar/events", eventBody("Mine again", ten, 30, board.ID)); code != http.StatusConflict || !bytes.Contains(raw, []byte("Budget review")) {
		t.Errorf("the refusal to Sam names his own event: %d %s", code, raw)
	}
	// A repeating event clashing on a later occurrence is refused on that date.
	weekly := eventBody("Weekly", ten.AddDate(0, 0, -7), 30, board.ID)
	weekly["recurrence"] = map[string]any{"rule": "FREQ=WEEKLY;COUNT=4"}
	code, raw = w.call(maya, "POST", "/calendar/events", weekly)
	_ = json.Unmarshal(raw, &clash)
	if code != http.StatusConflict || clash.Data.Date != ten.Format("2006-01-02") {
		t.Errorf("a series clashing on its second week: %d %s", code, raw)
	}
	// Moving an event onto a booking is refused too; adding a room on update works.
	code, raw = w.call(maya, "POST", "/calendar/events", eventBody("Later", ten.Add(3*time.Hour), 30))
	later := data[struct {
		ID string `json:"id"`
	}](t, raw)
	if code, raw := w.patch(maya, later.ID, map[string]any{"rooms": []string{board.ID}}); code != http.StatusOK {
		t.Fatalf("a room added on update: %d %s", code, raw)
	}
	if code, raw := w.patch(maya, later.ID, map[string]any{"start": map[string]any{"at": ten, "zone": "UTC"},
		"end": map[string]any{"at": ten.Add(30 * time.Minute), "zone": "UTC"}}); code != http.StatusConflict {
		t.Errorf("moving a booked event onto another booking: %d %s", code, raw)
	}
	if code, _ := w.patch(maya, booked.ID, map[string]any{"rooms": []string{huddle.ID}}, sam); code == http.StatusOK {
		t.Errorf("Maya changed rooms on an event she may not edit")
	}

	// The room's calendar: busy only, no title and no room names, to Maya.
	q := fmt.Sprintf("?from=%s&to=%s", day.Format(time.RFC3339), day.Add(24*time.Hour).Format(time.RFC3339))
	code, raw = w.call(maya, "GET", "/calendar/rooms/"+board.ID+"/bookings"+q, nil)
	cal := data[struct {
		Room     roomRead          `json:"room"`
		Bookings []roomBookingRead `json:"bookings"`
	}](t, raw)
	if code != http.StatusOK || len(cal.Bookings) != 2 {
		t.Fatalf("the room's calendar: %d %s", code, raw)
	}
	for _, b := range cal.Bookings {
		if b.EventID == booked.ID || b.Title == "Budget review" || bytes.Contains(raw, []byte("Budget review")) {
			t.Errorf("Maya sees Sam's event on the room: %s", raw)
		}
		if b.EventID != later.ID && (!b.Busy || len(b.Rooms) != 0) {
			t.Errorf("a busy-only booking shows its rooms or isn't busy: %+v", b)
		}
	}
	if _, raw := w.call(sam, "GET", "/calendar/rooms/"+board.ID+"/bookings"+q, nil); !bytes.Contains(raw, []byte("Budget review")) {
		t.Errorf("Sam doesn't see his own booking: %s", raw)
	}

	// Free rooms for a slot, by capacity.
	slot := fmt.Sprintf("?from=%s&to=%s&capacity=", ten.Format(time.RFC3339), ten.Add(time.Hour).Format(time.RFC3339))
	code, raw = w.call(maya, "GET", "/calendar/rooms/free"+slot+"4", nil)
	free := data[[]roomRead](t, raw)
	if code != http.StatusOK || len(free) != 1 || free[0].ID != old.ID {
		t.Errorf("free rooms for 4 at ten: %d %s (want only Old wing)", code, raw)
	}

	// Find a time counts a named room as busy.
	code, raw = w.call(maya, "POST", "/event/suggestTimes", map[string]any{"participants": []string{}, "duration_minutes": 60,
		"from": ten, "to": ten.Add(2 * time.Hour), "rooms": []string{board.ID}})
	var sugg struct {
		Suggestions []struct {
			Start time.Time `json:"start"`
		} `json:"suggestions"`
		Data struct {
			Suggestions []struct {
				Start time.Time `json:"start"`
			} `json:"suggestions"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &sugg)
	all := append(sugg.Suggestions, sugg.Data.Suggestions...)
	if code != http.StatusOK || len(all) == 0 {
		t.Fatalf("suggestTimes with a room: %d %s", code, raw)
	}
	for _, s := range all {
		if s.Start.Before(ten.Add(time.Hour)) {
			t.Errorf("suggested %s while the room is booked", s.Start)
		}
	}

	// Series edits hold the room in their own write: moving one instance, or
	// the whole series, onto a booking is refused; this and following keeps
	// the series' room; an instance alone can't change the series' rooms.
	tue, at := ten.AddDate(0, 0, 1), func(d time.Time) map[string]any { return map[string]any{"at": d, "zone": "UTC"} }
	if code, raw := w.call(sam, "POST", "/calendar/events", eventBody("Huddle hold", tue.AddDate(0, 0, 1), 30, huddle.ID)); code != http.StatusCreated {
		t.Fatalf("the huddle booking: %d %s", code, raw)
	}
	series := eventBody("Standup", tue, 30, huddle.ID)
	series["recurrence"] = map[string]any{"rule": "FREQ=WEEKLY;COUNT=4"}
	code, raw = w.call(maya, "POST", "/calendar/events", series)
	standup := data[struct {
		ID string `json:"id"`
	}](t, raw)
	if code != http.StatusCreated {
		t.Fatalf("a weekly series in the huddle: %d %s", code, raw)
	}
	inst := func(week int) string { return standup.ID + "_" + tue.AddDate(0, 0, 7*week).Format("20060102T150405Z") }
	onto := tue.AddDate(0, 0, 1)
	for name, body := range map[string]map[string]any{
		"one instance": {"scope": "this", "start": at(onto), "end": at(onto.Add(30 * time.Minute))},
		"the series":   {"scope": "all", "start": at(onto), "end": at(onto.Add(30 * time.Minute))},
	} {
		code, raw := w.patch(maya, inst(1), body)
		clash.Code, clash.Data.Room = "", ""
		_ = json.Unmarshal(raw, &clash)
		if code != http.StatusConflict || clash.Code != "room_taken" || clash.Data.Room != "Huddle" {
			t.Errorf("%s moved onto the huddle's booking: %d %s", name, code, raw)
		}
	}
	if code, raw := w.patch(maya, inst(1), map[string]any{"scope": "this", "title": "Standup, elsewhere", "rooms": []string{old.ID}}); code != http.StatusBadRequest {
		t.Errorf("an instance changing its series' rooms: %d %s", code, raw)
	}
	if code, raw := w.patch(maya, inst(2), map[string]any{"scope": "following", "title": "Standup, later"}); code != http.StatusOK {
		t.Fatalf("this and following: %d %s", code, raw)
	}
	if code, raw := w.call(sam, "POST", "/calendar/events", eventBody("Into the tail", tue.AddDate(0, 0, 21), 30, huddle.ID)); code != http.StatusConflict {
		t.Errorf("the split-off series let its room go: %d %s", code, raw)
	}
	// A restored event holds its rooms again: one booked while it was in the
	// trash is let go, and the answer says so.
	code, raw = w.call(maya, "POST", "/calendar/events", eventBody("Restorable", tue.Add(4*time.Hour), 30, huddle.ID))
	gone := data[struct {
		ID string `json:"id"`
	}](t, raw)
	if code, raw := w.call(maya, "DELETE", "/calendar/events/"+gone.ID, nil); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, raw)
	}
	if code, raw := w.call(sam, "POST", "/calendar/events", eventBody("Took the slot", tue.Add(4*time.Hour), 30, huddle.ID)); code != http.StatusCreated {
		t.Fatalf("booking a trashed event's slot: %d %s", code, raw)
	}
	code, raw = w.call(maya, "POST", "/calendar/events/"+gone.ID+"/restore", nil)
	if code != http.StatusOK || len(data[struct {
		RoomsLetGo []roomRead `json:"rooms_let_go"`
	}](t, raw).RoomsLetGo) != 1 {
		t.Errorf("a restore onto a taken room: %d %s, want 200 and the rooms let go", code, raw)
	}

	// A retired room keeps its bookings and takes no new one.
	if code, raw := w.call(ada, "POST", "/admin/calendar/rooms/"+board.ID+"/retire", nil); code != http.StatusOK || !data[roomRead](t, raw).Retired {
		t.Fatalf("retire: %d %s", code, raw)
	}
	if code, _ := w.call(sam, "POST", "/calendar/events", eventBody("After retiring", ten.Add(5*time.Hour), 30, board.ID)); code != http.StatusBadRequest && code != http.StatusConflict {
		t.Errorf("a retired room took a booking: %d", code)
	}
	if _, raw := w.call(sam, "GET", "/calendar/rooms/"+board.ID+"/bookings"+q, nil); !bytes.Contains(raw, []byte("Budget review")) {
		t.Errorf("a retired room lost its bookings: %s", raw)
	}
	var audits int
	if err := postgresInit.DBConn.SqlDB.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE summary ILIKE '%room%'`).Scan(&audits); err != nil || audits < 5 {
		t.Errorf("room changes in the audit log: %d (%v), want 5", audits, err)
	}
}

// Two writers book one room for overlapping times at once (goroutines
// released together, real Postgres), round after round: each round exactly
// one wins, the other is refused, and the room holds one live booking for it.
// Sessions are warmed first and the race repeats so the writers meet inside
// the write; without the room lock a round double-books.
func TestTwoWritersBookingOneRoomAtOnceOneWins(t *testing.T) {
	w, p := roomsSetup(t)
	code, raw := w.call(p["Ada"], "POST", "/admin/calendar/rooms", map[string]any{"name": "Contested", "capacity": 4})
	room := data[roomRead](t, raw)
	if code != http.StatusCreated || room.ID == "" {
		t.Fatalf("admin adds the contested room: %d %s", code, raw)
	}
	writers := []uuid.UUID{p["Sam"], p["Maya"]}
	for _, who := range writers {
		if code, raw := w.call(who, "GET", "/calendar/rooms", nil); code != http.StatusOK {
			t.Fatalf("a writer's first call: %d %s", code, raw)
		}
	}
	const rounds = 12
	first := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 3).Add(10 * time.Hour)
	for round := range rounds {
		ten := first.AddDate(0, 0, round)
		var wg sync.WaitGroup
		gate := make(chan struct{})
		codes := make([]int, len(writers))
		for i, who := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				codes[i], _ = w.call(who, "POST", "/calendar/events", eventBody(fmt.Sprintf("Writer %d, round %d", i, round), ten.Add(time.Duration(i)*15*time.Minute), 60, room.ID))
			}()
		}
		close(gate)
		wg.Wait()
		won, refused := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusCreated:
				won++
			case http.StatusConflict:
				refused++
			}
		}
		var live int
		if err := postgresInit.DBConn.SqlDB.QueryRow(`SELECT count(*) FROM calendar_room_bookings
			WHERE room_id = $1 AND deleted_at IS NULL AND starts_at >= $2 AND starts_at < $3`,
			room.ID, ten.Add(-time.Hour), ten.Add(2*time.Hour)).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if won != 1 || refused != 1 || live != 1 {
			t.Fatalf("round %d, two writers at once: codes %v, %d live bookings; want one 201, one 409, one booking", round, codes, live)
		}
	}
}
