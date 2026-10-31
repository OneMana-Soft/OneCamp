//go:build integration

package integration_test

// A meeting's lobby (calls CALLS-2): a guest knocks and waits; whoever runs
// the call admits or denies them; a ticket opens nothing through another
// link; a meeting can let anyone with the link in at once; and a knock nobody
// answers lapses.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callInviteBusiness "github.com/akashc777/OneCamp/business/CallInvite"
	callLobbyBusiness "github.com/akashc777/OneCamp/business/CallLobby"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/akashc777/OneCamp/tests/integration"
)

func callLobbyMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_call_lobby."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one call_lobby %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The lobby's migration goes down and up again on Postgres 12, and its checks hold.
func TestCallLobbyMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	exists := func(table string) bool {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_name = $1`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if _, err := env.PG.Exec(callLobbyMigration(t, "down")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if exists("call_meetings") || exists("call_knocks") {
		t.Fatal("the lobby's tables are still there after down")
	}
	if _, err := env.PG.Exec(callLobbyMigration(t, "up")); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if !exists("call_meetings") || !exists("call_knocks") {
		t.Fatal("the lobby's tables are missing after up")
	}
	if _, err := env.PG.Exec(`INSERT INTO call_meetings (id, owner, lobby, created_at) VALUES ('m1', $1, 'everyone', NOW())`, uuid.New()); err == nil {
		t.Error("a lobby choice the meeting doesn't know was stored")
	}
}

func TestCallLobbyLetsGuestsInOnlyWhenSomeoneRunningTheCallSaysSo(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	lk := integration.SetupLiveKit(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}

	people := seedChannel(t, pg, dg, uuid.NewString(), "maya")
	meeting, err := guestBusiness.CreateInstantMeeting(ctx, people["maya"], "")
	if err != nil {
		t.Fatal(err)
	}
	if err := callLobbyBusiness.SaveMeeting(ctx, meeting.MeetingID, uuid.MustParse(people["maya"].Uuid)); err != nil {
		t.Fatal(err)
	}
	host := join(t, lk, meeting.HostToken)
	joinSettled(t, meeting.Room, people["maya"].Uid, host)
	connected(t, ctx, meeting.Room, people["maya"].Uid)
	s, err := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: callStateBusiness.KindMeeting, ID: meeting.MeetingID})
	if err != nil || s == nil {
		t.Fatalf("no meeting recorded: %v", err)
	}
	door, err := guestBusiness.OpenDoor(ctx, meeting.GuestToken)
	if err != nil {
		t.Fatal(err)
	}
	grant := door.Grant

	if waits, err := callLobbyBusiness.Waits(ctx, meeting.Room, false); err != nil || !waits {
		t.Fatalf("a new meeting's guests wait: %v, %v", waits, err)
	}
	// An instant meeting is no event's, so it has no invitees: everyone waits.
	if waits, err := callLobbyBusiness.Waits(ctx, meeting.Room, true); err != nil || !waits {
		t.Fatalf("an instant meeting let someone in as an invitee: waits %v, %v", waits, err)
	}
	// The guest's page names the call: an instant meeting has no title, so it is its host's.
	if got := callLobbyBusiness.Title(ctx, meeting.Room, uuid.MustParse(people["maya"].Uuid)); got != "Maya's meeting" {
		t.Errorf("the meeting's name for a guest: %q", got)
	}
	gia, err := callLobbyBusiness.Knock(ctx, meeting.Room, grant.Id, "Gia")
	if err != nil {
		t.Fatal(err)
	}
	ben, _ := callLobbyBusiness.Knock(ctx, meeting.Room, grant.Id, "Ben")
	cy, _ := callLobbyBusiness.Knock(ctx, meeting.Room, grant.Id, "Cy")
	waiting, _ := callLobbyBusiness.WaitingIn(ctx, meeting.Room)
	if len(waiting) != 3 || waiting[0].Name != "Gia" {
		t.Fatalf("the lobby: %+v", waiting)
	}

	act := func(a callControlBusiness.Action) (callControlBusiness.Result, error) {
		return callControlBusiness.Act(ctx, people["maya"], s.ID, a)
	}
	if _, err := act(callControlBusiness.Action{Action: callControlBusiness.ActAdmit, Ticket: gia.String()}); err != nil {
		t.Fatal(err)
	}
	if ok, name, err := callLobbyBusiness.Answer(ctx, gia, grant.Id); err != nil || !ok || name != "Gia" {
		t.Fatalf("Gia's answer: %v %q %v", ok, name, err)
	}
	if _, _, err := callLobbyBusiness.Answer(ctx, gia, uuid.New()); !errors.Is(err, callLobbyBusiness.ErrNoKnock) {
		t.Errorf("a ticket through another link: %v", err)
	}
	if _, err := act(callControlBusiness.Action{Action: callControlBusiness.ActAdmit, Ticket: gia.String()}); !errors.Is(err, callControlBusiness.ErrNotWaiting) {
		t.Errorf("admitting twice: %v", err)
	}
	if _, err := act(callControlBusiness.Action{Action: callControlBusiness.ActDeny, Ticket: ben.String()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := callLobbyBusiness.Answer(ctx, ben, grant.Id); !errors.Is(err, callLobbyBusiness.ErrDenied) {
		t.Errorf("Ben's answer: %v, want denied", err)
	}
	res, err := act(callControlBusiness.Action{Action: callControlBusiness.ActAdmitAll})
	if err != nil || res.Affected != 1 {
		t.Fatalf("admit all: %+v %v; want Cy", res, err)
	}
	if ok, _, _ := callLobbyBusiness.Answer(ctx, cy, grant.Id); !ok {
		t.Error("Cy wasn't let in by admit all")
	}

	// Anyone with the link joins at once.
	if found, err := callLobbyBusiness.SetLobby(ctx, meeting.MeetingID, false); err != nil || !found {
		t.Fatalf("set lobby: %v %v", found, err)
	}
	if waits, _ := callLobbyBusiness.Waits(ctx, meeting.Room, false); waits {
		t.Error("guests still wait in a meeting that lets anyone in")
	}

	// A knock nobody answers lapses.
	old := callStateModel.Knock{ID: uuid.New(), Room: meeting.Room, GrantID: grant.Id, Name: "Late", KnockedAt: time.Now().Add(-time.Hour)}
	if err := callStateModel.AddKnock(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := callLobbyBusiness.ExpireKnocks(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := callLobbyBusiness.Answer(ctx, old.ID, grant.Id); !errors.Is(err, callLobbyBusiness.ErrDenied) {
		t.Errorf("a lapsed knock: %v", err)
	}
}

// V-82 (CR's should-fix): who waits at a meeting's door is read only by those
// who may let them in. The host and a co-host see the knocker in the call's
// read (/calls/current); a colleague in the call with them, in by the meeting's
// link, gets no waiting list, and someone who never came in can't read the
// call at all.
func TestOnlyWhoMayAdmitReadsWhoWaitsAtAMeetingsDoor(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	lk := integration.SetupLiveKit(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = settingsBusiness.SetGuestAccessEnabled(false) })

	people := seedChannel(t, pg, dg, uuid.NewString(), "maya", "sam", "lee", "outsider")
	meeting, err := guestBusiness.CreateInstantMeeting(ctx, people["maya"], "")
	if err != nil {
		t.Fatal(err)
	}
	if err := callLobbyBusiness.SaveMeeting(ctx, meeting.MeetingID, uuid.MustParse(people["maya"].Uuid)); err != nil {
		t.Fatal(err)
	}
	enter := func(name, token string) {
		t.Helper()
		joinSettled(t, meeting.Room, people[name].Uid, join(t, lk, token))
		connected(t, ctx, meeting.Room, people[name].Uid)
	}
	enter("maya", meeting.HostToken)
	// Sam and Lee, colleagues, come in at the meeting's link as themselves.
	for _, name := range []string{"sam", "lee"} {
		entry, err := callInviteBusiness.Enter(ctx, callInviteBusiness.Door{Token: meeting.GuestToken}, people[name], "")
		if err != nil || entry.Token == "" {
			t.Fatalf("%s at the meeting's link: %+v, %v", name, entry, err)
		}
		enter(name, entry.Token)
	}
	s, err := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: callStateBusiness.KindMeeting, ID: meeting.MeetingID})
	if err != nil || s == nil {
		t.Fatalf("no meeting recorded: %v", err)
	}
	if _, err := callControlBusiness.Act(ctx, people["maya"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActMakeCohost, Identity: people["sam"].Uid}); err != nil {
		t.Fatal(err)
	}
	door, err := guestBusiness.OpenDoor(ctx, meeting.GuestToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callLobbyBusiness.Knock(ctx, meeting.Room, door.Grant.Id, "Gia"); err != nil {
		t.Fatal(err)
	}

	target := "/calls/current?kind=meeting&id=" + meeting.MeetingID
	type read struct {
		You struct {
			Role string   `json:"role"`
			Can  []string `json:"can"`
		} `json:"you"`
		Waiting []callLobbyBusiness.Waiting `json:"waiting"`
	}
	readAs := func(name string) (got read, listed bool) {
		t.Helper()
		code, body := callRead(t, callsController.CurrentCall, http.MethodGet, target, people[name], "", "")
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body["data"], &fields); code != http.StatusOK || err != nil {
			t.Fatalf("%s reads the meeting: %d %s %v", name, code, body["msg"], err)
		}
		_, listed = fields["waiting"]
		if err := json.Unmarshal(body["data"], &got); err != nil {
			t.Fatal(err)
		}
		return got, listed
	}
	for name, role := range map[string]string{"maya": callStateModel.RoleHost, "sam": callStateModel.RoleCohost} {
		got, _ := readAs(name)
		if got.You.Role != role || len(got.Waiting) != 1 || got.Waiting[0].Name != "Gia" {
			t.Errorf("%s reads who waits at the door as %q: %+v", name, got.You.Role, got.Waiting)
		}
	}
	got, listed := readAs("lee")
	if got.You.Role == callStateModel.RoleHost || got.You.Role == callStateModel.RoleCohost || slices.Contains(got.You.Can, string(callControlBusiness.CanAdmit)) {
		t.Fatalf("Lee came in by the link and runs the call: %+v", got.You)
	}
	if listed || len(got.Waiting) != 0 {
		t.Errorf("Lee, in the call as %q, reads who waits at the door: %+v", got.You.Role, got.Waiting)
	}
	if code, _ := callRead(t, callsController.CurrentCall, http.MethodGet, target, people["outsider"], "", ""); code != http.StatusNotFound {
		t.Errorf("someone who never came in read the meeting: %d", code)
	}
}
