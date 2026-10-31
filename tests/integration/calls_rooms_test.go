//go:build integration

package integration_test

// Personal meeting rooms (calls CALLS-2, D-CL-35): one per person, at their
// name, standing under the workspace's link cap; a passcode, a lobby, a new
// address; off with "Personal meeting rooms" or guest access; closed by its
// owner, after which their name is free for their next room.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	callLobbyBusiness "github.com/akashc777/OneCamp/business/CallLobby"
	callRoomBusiness "github.com/akashc777/OneCamp/business/CallRoom"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func callRoomsMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_call_rooms."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one call_rooms %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The rooms' migration goes down and up again on Postgres 12, and keeps each
// person to one open room, never limiting their instant meetings.
func TestCallRoomsMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	owner := uuid.New()
	meeting := func(id, kind string) error {
		_, err := env.PG.Exec(`INSERT INTO call_meetings (id, kind, owner, lobby, created_at) VALUES ($1, $2, $3, 'guests', NOW())`, id, kind, owner)
		return err
	}
	if _, err := env.PG.Exec(callRoomsMigration(t, "down")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := env.PG.Exec(callRoomsMigration(t, "up")); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := meeting("r1", "personal"); err != nil {
		t.Fatal(err)
	}
	if err := meeting("r2", "personal"); err == nil {
		t.Error("a second open room for one person")
	}
	if meeting("i1", "instant") != nil || meeting("i2", "instant") != nil {
		t.Error("instant meetings were limited")
	}
	if _, err := env.PG.Exec(`UPDATE call_meetings SET archived_at = NOW() WHERE id = 'r1'`); err != nil {
		t.Fatal(err)
	}
	if err := meeting("r3", "personal"); err != nil {
		t.Errorf("a room after the first was closed: %v", err)
	}
}

func TestCallRoomsStandAtTheirOwnersAddress(t *testing.T) {
	ctx := context.Background()
	// A passcode's unlock is signed with the server's secret.
	t.Setenv("JWT_SECRET", "call-rooms-integration-secret")
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	integration.SetupLiveKit(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := settingsBusiness.SetGuestRoomsEnabled(true); err != nil {
		t.Fatal(err)
	}
	people := seedChannel(t, pg, dg, uuid.NewString(), "maya", "ben", "outsider")
	maya := uuid.MustParse(people["maya"].Uuid)

	// The routes as router.go and router/callsRooms.go mount them.
	pub := chi.NewRouter()
	pub.Get("/public/r/{slug}", guestController.RoomAtAddress)
	pub.Post("/public/r/{slug}/unlock", guestController.UnlockRoom)
	pub.Post("/public/r/{slug}/join", guestController.JoinRoom)
	pub.Get("/public/r/{slug}/knock/{ticket}", guestController.RoomKnockAnswer)
	mem := chi.NewRouter()
	mem.Get("/calls/room", callsController.GetMyRoom)
	mem.Post("/calls/room", callsController.OpenMyRoom)
	mem.Patch("/calls/room", callsController.ChangeMyRoom)
	mem.Delete("/calls/room", callsController.CloseMyRoom)
	mem.Get("/meet/instant/{meeting_id}", guestController.GetInstantMeeting)
	call := func(h http.Handler, method, target, body string, who *dgraphStruct.DgraphUser) (int, map[string]json.RawMessage) {
		t.Helper()
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.RemoteAddr = "203.0.113.9:40000"
		if who != nil {
			info := userModels.UserInfo{UserDgraphInfo: *who}
			info.UserPostgresInfo.Id = uuid.MustParse(who.Uuid)
			r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, info))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out map[string]json.RawMessage
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	roomOf := func(out map[string]json.RawMessage) callRoomBusiness.Room {
		t.Helper()
		var r callRoomBusiness.Room
		if err := json.Unmarshal(out["data"], &r); err != nil {
			t.Fatalf("a room: %s (%v)", out["data"], err)
		}
		return r
	}
	read := func(out map[string]json.RawMessage) map[string]any {
		var m map[string]any
		_ = json.Unmarshal(out["data"], &m)
		return m
	}
	str := func(raw json.RawMessage) string {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}

	// Maya opens her room: at her name, guests waiting; opening again is the same room.
	code, out := call(mem, http.MethodPost, "/calls/room", "", people["maya"])
	room := roomOf(out)
	if code != http.StatusOK || room.Address != "/r/maya" || room.Slug != "maya" || !room.GuestsWait || room.Passcode || !room.On {
		t.Fatalf("Maya's room: %d %+v", code, room)
	}
	if _, out = call(mem, http.MethodPost, "/calls/room", "", people["maya"]); roomOf(out).MeetingID != room.MeetingID {
		t.Errorf("opened again: %s", out["data"])
	}
	if m, err := callStateModel.MeetingOf(ctx, room.MeetingID); err != nil || m == nil || m.Kind != callStateModel.KindPersonal || m.Owner != maya {
		t.Fatalf("its meeting: %+v, %v", m, err)
	}
	// Its page names it.
	code, out = call(pub, http.MethodGet, "/public/r/maya", "", nil)
	if page := read(out); code != http.StatusOK || page["state"] != "live" || page["title"] != "Maya's room" {
		t.Errorf("its page: %d %v", code, page)
	}

	// Under a 30-day link cap, the room still answers on day 31.
	if _, err := policyBusiness.Admin.Set(ctx, policyBusiness.GuestLinkMaxDays.Key, json.RawMessage("30"), policyBusiness.Changer{UserID: maya, Email: "maya@example.test"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = policyBusiness.Admin.Set(context.Background(), policyBusiness.GuestLinkMaxDays.Key, json.RawMessage("0"), policyBusiness.Changer{UserID: maya})
	})
	if days := guestBusiness.LinkCapDays(ctx); days != 30 {
		t.Fatalf("the cap: %d days", days)
	}
	if _, err := pg.PG.Exec(`UPDATE guest_grants SET created_at = NOW() - interval '31 days' WHERE slug = 'maya'`); err != nil {
		t.Fatal(err)
	}
	if code, out = call(pub, http.MethodGet, "/public/r/maya", "", nil); code != http.StatusOK || read(out)["state"] != "live" {
		t.Errorf("on day 31 under a 30-day cap: %d %s", code, out["data"])
	}

	// Another member of the same name gets her name and some letters.
	other := uuid.New()
	if _, err := pg.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, 'maya.two@example.test', 'maya-two', 'Maya', NOW(), NOW())`, other); err != nil {
		t.Fatal(err)
	}
	second, err := callRoomBusiness.Open(ctx, other)
	if err != nil || !strings.HasPrefix(second.Slug, "maya-") || len(second.Slug) != len("maya-")+8 || second.MeetingID == room.MeetingID {
		t.Errorf("the second Maya's room: %+v, %v", second, err)
	}

	// An address another link has is taken: refused inside the change's
	// transaction, and nothing changes.
	if code, out = call(mem, http.MethodPost, "/calls/room", "", people["ben"]); code != http.StatusOK || roomOf(out).Address != "/r/ben" {
		t.Fatalf("Ben's room: %d %s", code, out["data"])
	}
	code, out = call(mem, http.MethodPatch, "/calls/room", `{"slug":"ben","guests_wait":false}`, people["maya"])
	if code != http.StatusConflict || str(out["code"]) != "taken" {
		t.Errorf("Ben's address for Maya's room: %d %v", code, out)
	}
	if _, out = call(mem, http.MethodGet, "/calls/room", "", people["maya"]); roomOf(out).Slug != "maya" || !roomOf(out).GuestsWait {
		t.Errorf("a refused change changed the room: %s", out["data"])
	}

	// The admin's list says whose each room is, the two Mayas apart by address.
	adm := chi.NewRouter()
	adm.Get("/admin/guest-grants", guestController.ListGuestGrants)
	code, out = call(adm, http.MethodGet, "/admin/guest-grants", "", people["maya"])
	var listed []map[string]any
	_ = json.Unmarshal(out["data"], &listed)
	whose := map[string]any{}
	for _, g := range listed {
		if slug, _ := g["slug"].(string); slug != "" {
			whose[slug] = g["created_by_name"]
		}
	}
	if code != http.StatusOK || whose["maya"] != "Maya" || whose["ben"] != "Ben" || whose[second.Slug] != "Maya" {
		t.Errorf("the admin's list: %d %v", code, whose)
	}

	// A passcode: the page says locked and nothing more, the door asks for
	// it, a wrong one opens nothing, the right one opens the room.
	if code, _ = call(mem, http.MethodPatch, "/calls/room", `{"passcode":"432198"}`, people["maya"]); code != http.StatusOK {
		t.Fatalf("a passcode: %d", code)
	}
	code, out = call(pub, http.MethodGet, "/public/r/maya", "", nil)
	if page := read(out); code != http.StatusOK || page["state"] != "locked" || page["title"] != nil {
		t.Errorf("a locked room's page: %d %v", code, page)
	}
	if code, out = call(pub, http.MethodPost, "/public/r/maya/join", `{"display_name":"Gia"}`, nil); code != http.StatusUnauthorized || str(out["code"]) != "locked" {
		t.Errorf("joining without the passcode: %d %v", code, out)
	}
	if code, _ = call(pub, http.MethodPost, "/public/r/maya/unlock", `{"passcode":"1234"}`, nil); code != http.StatusUnauthorized {
		t.Errorf("a wrong passcode: %d", code)
	}
	code, out = call(pub, http.MethodPost, "/public/r/maya/unlock", `{"passcode":"432198"}`, nil)
	unlock, _ := read(out)["token"].(string)
	if code != http.StatusOK || unlock == "" {
		t.Fatalf("the right passcode: %d %s", code, out["data"])
	}
	k := "?k=" + url.QueryEscape(unlock)
	if code, out = call(pub, http.MethodGet, "/public/r/maya"+k, "", nil); code != http.StatusOK || read(out)["state"] != "live" || read(out)["title"] != "Maya's room" {
		t.Errorf("unlocked: %d %s", code, out["data"])
	}

	// Gia knocks and waits; Maya lets her in, and her page gets her call token.
	code, out = call(pub, http.MethodPost, "/public/r/maya/join"+k, `{"display_name":"Gia"}`, nil)
	ticket, _ := read(out)["ticket"].(string)
	if code != http.StatusAccepted || ticket == "" {
		t.Fatalf("Gia's knock: %d %s", code, out["data"])
	}
	if _, err := callLobbyBusiness.Decide(ctx, room.Room, ticket, true, people["maya"].Uid); err != nil {
		t.Fatal(err)
	}
	code, out = call(pub, http.MethodGet, "/public/r/maya/knock/"+ticket+k, "", nil)
	if code != http.StatusOK || read(out)["token"] == nil || read(out)["room"] != room.Room {
		t.Errorf("Gia let in: %d %s", code, out["data"])
	}
	// A new passcode ends every unlock given before.
	if code, _ = call(mem, http.MethodPatch, "/calls/room", `{"passcode":"999999"}`, people["maya"]); code != http.StatusOK {
		t.Fatal(code)
	}
	if code, out = call(pub, http.MethodGet, "/public/r/maya"+k, "", nil); read(out)["state"] != "locked" {
		t.Errorf("the old unlock after a new passcode: %d %s", code, out["data"])
	}

	// No passcode and no lobby: anyone at the address joins at once.
	if code, _ = call(mem, http.MethodPatch, "/calls/room", `{"passcode":"","guests_wait":false}`, people["maya"]); code != http.StatusOK {
		t.Fatal(code)
	}
	if code, out = call(pub, http.MethodPost, "/public/r/maya/join", `{"display_name":"Ben's client"}`, nil); code != http.StatusOK || read(out)["token"] == nil {
		t.Errorf("joining an open room: %d %s", code, out["data"])
	}

	// A new address: the old one stops answering at once.
	code, out = call(mem, http.MethodPatch, "/calls/room", `{"slug":"Maya Standup"}`, people["maya"])
	if code != http.StatusOK || roomOf(out).Address != "/r/maya-standup" {
		t.Fatalf("a new address: %d %s", code, out["data"])
	}
	if code, _ = call(pub, http.MethodGet, "/public/r/maya", "", nil); code != http.StatusNotFound {
		t.Errorf("the old address: %d", code)
	}
	// The host's own page shows the address, not an end; another one-call
	// link, turning off the earlier ones, leaves the room's address alone.
	code, out = call(mem, http.MethodGet, "/meet/instant/"+room.MeetingID, "", people["maya"])
	if page := read(out); code != http.StatusOK || page["address"] != "/r/maya-standup" {
		t.Errorf("the host's page: %d %v", code, page)
	}
	if _, err := guestBusiness.NewMeetingLink(ctx, room.Room, people["maya"], true); err != nil {
		t.Fatal(err)
	}
	if code, _ = call(pub, http.MethodGet, "/public/r/maya-standup", "", nil); code != http.StatusOK {
		t.Errorf("the room after its earlier links were turned off: %d", code)
	}

	// Rooms off: every address stops answering, and a room's guest can't
	// come back on a token they already hold; on again, it answers.
	links, err := guestModelLinks(ctx, room.Room)
	if err != nil || links.room == uuid.Nil || links.oneCall == uuid.Nil {
		t.Fatalf("the room's links: %+v, %v", links, err)
	}
	if err := settingsBusiness.SetGuestRoomsEnabled(false); err != nil {
		t.Fatal(err)
	}
	if code, _ = call(pub, http.MethodGet, "/public/r/maya-standup", "", nil); code != http.StatusNotFound {
		t.Errorf("an address with rooms off: %d", code)
	}
	if code, _ = call(pub, http.MethodPost, "/public/r/maya-standup/join", `{"display_name":"Gia"}`, nil); code != http.StatusNotFound {
		t.Errorf("joining with rooms off: %d", code)
	}
	if ok, err := guestBusiness.GuestMayJoin(ctx, room.Room, "guest-"+links.room.String()+"-a1b2c3"); err != nil || ok {
		t.Errorf("a room's guest with rooms off: %v, %v", ok, err)
	}
	if ok, err := guestBusiness.GuestMayJoin(ctx, room.Room, "guest-"+links.oneCall.String()+"-a1b2c3"); err != nil || !ok {
		t.Errorf("a one-call link's guest with rooms off: %v, %v", ok, err)
	}
	guestBusiness.RemoveRoomGuests(ctx)
	if _, err := callRoomBusiness.Open(ctx, maya); err == nil {
		t.Error("a room opened with rooms off")
	}
	if err := settingsBusiness.SetGuestRoomsEnabled(true); err != nil {
		t.Fatal(err)
	}
	if code, _ = call(pub, http.MethodGet, "/public/r/maya-standup", "", nil); code != http.StatusOK {
		t.Errorf("rooms on again: %d", code)
	}

	// Closed: the address and every link to the room end, and opening again
	// is a new room at her name.
	if code, _ = call(mem, http.MethodDelete, "/calls/room", "", people["maya"]); code != http.StatusOK {
		t.Fatalf("closing: %d", code)
	}
	if code, _ = call(pub, http.MethodGet, "/public/r/maya-standup", "", nil); code != http.StatusNotFound {
		t.Errorf("a closed room's address: %d", code)
	}
	if ok, _ := guestBusiness.GuestMayJoin(ctx, room.Room, "guest-"+links.oneCall.String()+"-a1b2c3"); ok {
		t.Error("a closed room's one-call link still lets its guest in")
	}
	if _, out = call(mem, http.MethodGet, "/calls/room", "", people["maya"]); string(out["data"]) != "null" {
		t.Errorf("a closed room is still hers: %s", out["data"])
	}
	code, out = call(mem, http.MethodPost, "/calls/room", "", people["maya"])
	if again := roomOf(out); code != http.StatusOK || again.MeetingID == room.MeetingID || again.Address != "/r/maya" {
		t.Errorf("a room after closing: %d %+v", code, again)
	}
}

// roomLinks are a room's live links: its address's, and a one-call link.
type roomLinks struct{ room, oneCall uuid.UUID }

func guestModelLinks(ctx context.Context, room string) (roomLinks, error) {
	var out roomLinks
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `SELECT id, slug IS NOT NULL FROM guest_grants WHERE resource_type = 'meeting' AND resource_id = $1 AND revoked_at IS NULL`, room)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var standing bool
		if err := rows.Scan(&id, &standing); err != nil {
			return out, err
		}
		if standing {
			out.room = id
		} else {
			out.oneCall = id
		}
	}
	return out, rows.Err()
}
