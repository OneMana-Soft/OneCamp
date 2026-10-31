//go:build integration

package integration_test

// Hands and nonverbal feedback in a call (calls CALLS-3a): the queue keeps
// its order across a reconnect, only the server sets what the room sees, the
// people running a call lower others' hands and nobody else does, and a guest
// acts only on themselves, proved by their own call token.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func callHandsMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_call_hands."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one call_hands %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The hands migration goes down and up again on Postgres 12, and its check
// holds: only the four kinds of feedback are stored.
func TestCallHandsMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	column := func(name string) bool {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'call_attendance' AND column_name = $1`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if _, err := env.PG.Exec(callHandsMigration(t, "down")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if column("hand_raised_at") || column("feedback") {
		t.Fatal("the hands columns are still there after down")
	}
	if _, err := env.PG.Exec(callHandsMigration(t, "up")); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if !column("hand_raised_at") || !column("feedback") {
		t.Fatal("the hands columns are missing after up")
	}
	session := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at) VALUES ($1, 'room-h', 'huddle', 'channel', 'room-h', NOW())`, session); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO call_attendance (session_id, identity, name, role, joined_at, last_joined_at, feedback) VALUES ($1, 'a', 'A', 'participant', NOW(), NOW(), 'slow_down')`, session); err != nil {
		t.Fatalf("feedback slow_down: %v", err)
	}
	if _, err := env.PG.Exec(`INSERT INTO call_attendance (session_id, identity, name, role, joined_at, last_joined_at, feedback) VALUES ($1, 'b', 'B', 'participant', NOW(), NOW(), 'shout')`, session); err == nil {
		t.Error("feedback the call doesn't know was stored")
	}
}

// Raised hands queue in order, keep their place across a reconnect, show on
// the person's attributes only as the server sets them, and come down for
// whoever runs the call. Feedback stays beside the name until taken away.
func TestCallHandsQueueAndFeedback(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	lk := integration.SetupLiveKit(t)

	room := uuid.NewString()
	// seedChannel leaves its last name out of the channel: an outsider, never let in.
	people := seedChannel(t, pg, dg, room, "sam", "ana", "jo", "outsider")
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	in := map[string]*joined{}
	enter := func(name string) {
		t.Helper()
		token, err := livekitInit.LiveKitService.GenerateToken(room, people[name].Uid, title(name), false, "", livekitInit.CallPublishSources)
		if err != nil {
			t.Fatal(err)
		}
		j := join(t, lk, token)
		joinSettled(t, room, people[name].Uid, j)
		connected(t, ctx, room, people[name].Uid)
		in[name] = j
	}
	enter("sam") // starts it, so hosts it
	enter("ana")
	enter("jo")
	s, err := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room})
	if err != nil || s == nil {
		t.Fatalf("no call recorded: %v", err)
	}
	act := func(name string, a callControlBusiness.Action) error {
		_, err := callControlBusiness.Act(ctx, people[name], s.ID, a)
		return err
	}
	attr := func(name, key string) string {
		p, _ := livekitInit.LiveKitService.Participant(ctx, room, people[name].Uid)
		if p == nil {
			return "?"
		}
		return p.Attributes[key]
	}
	raised := func(name string) *time.Time {
		got, _ := callStateBusiness.Get(ctx, s.ID)
		if p := got.Person(people[name].Uid); p != nil {
			return p.HandRaisedAt
		}
		return nil
	}

	if err := act("ana", callControlBusiness.Action{Action: callControlBusiness.ActRaiseHand}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := act("jo", callControlBusiness.Action{Action: callControlBusiness.ActRaiseHand}); err != nil {
		t.Fatal(err)
	}
	anaAt, joAt := raised("ana"), raised("jo")
	if anaAt == nil || joAt == nil || !anaAt.Before(*joAt) {
		t.Fatalf("the queue: Ana %v, Jo %v; want Ana first", anaAt, joAt)
	}
	eventually(t, "Ana's hand on her attributes", func() bool {
		return attr("ana", callControlBusiness.AttrHand) == strconv.FormatInt(anaAt.UnixMilli(), 10)
	})
	// Raising a raised hand keeps its place.
	if err := act("ana", callControlBusiness.Action{Action: callControlBusiness.ActRaiseHand}); err != nil {
		t.Fatal(err)
	}
	if again := raised("ana"); again == nil || !again.Equal(*anaAt) {
		t.Errorf("raised again, Ana's hand moved to %v from %v", again, anaAt)
	}

	// Jo drops out and comes back: the hand is still up, in its place, and
	// the room is told again.
	r, _ := livekitInit.LiveKitService.GetRoom(ctx, room)
	joInfo, _ := livekitInit.LiveKitService.Participant(ctx, room, people["jo"].Uid)
	in["jo"].room.Disconnect()
	callControlBusiness.OnLeft(ctx, room, r.Sid, joInfo, time.Now())
	enter("jo")
	if back := raised("jo"); back == nil || !back.Equal(*joAt) {
		t.Fatalf("after a reconnect Jo's hand is %v, want %v", back, joAt)
	}
	eventually(t, "Jo's hand on his attributes after the reconnect", func() bool {
		return attr("jo", callControlBusiness.AttrHand) == strconv.FormatInt(joAt.UnixMilli(), 10)
	})

	// A participant can't lower someone else's hand in a channel's call.
	if err := act("ana", callControlBusiness.Action{Action: callControlBusiness.ActLowerHand, Identity: people["jo"].Uid}); !errors.Is(err, callControlBusiness.ErrNotAllowed) {
		t.Errorf("Ana lowering Jo's hand: %v, want not allowed", err)
	}
	// Nor through an API token, even her own.
	if _, err := callControlBusiness.Act(helpers.WithActsThroughToken(ctx), people["ana"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActRaiseHand}); !errors.Is(err, callControlBusiness.ErrThroughToken) {
		t.Errorf("a hand through a token: %v", err)
	}
	// The host lowers Jo's, then everyone's.
	if err := act("sam", callControlBusiness.Action{Action: callControlBusiness.ActLowerHand, Identity: people["jo"].Uid}); err != nil {
		t.Fatal(err)
	}
	if raised("jo") != nil {
		t.Error("Jo's hand is still up after the host lowered it")
	}
	if err := act("sam", callControlBusiness.Action{Action: callControlBusiness.ActLowerHand, Identity: people["jo"].Uid}); !errors.Is(err, callControlBusiness.ErrHandDown) {
		t.Errorf("lowering a hand that is down: %v", err)
	}
	if err := act("jo", callControlBusiness.Action{Action: callControlBusiness.ActRaiseHand}); err != nil {
		t.Fatal(err)
	}
	res, err := callControlBusiness.Act(ctx, people["sam"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActLowerAll})
	if err != nil || res.Affected != 2 {
		t.Fatalf("lower all: %+v, %v; want 2", res, err)
	}
	eventually(t, "every hand off the attributes", func() bool {
		return attr("ana", callControlBusiness.AttrHand) == "" && attr("jo", callControlBusiness.AttrHand) == ""
	})

	// Feedback: set, refused when unknown, taken away.
	if err := act("jo", callControlBusiness.Action{Action: callControlBusiness.ActSetFeedback, Feedback: "slow_down"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Jo's feedback on his attributes", func() bool { return attr("jo", callControlBusiness.AttrFeedback) == "slow_down" })
	if err := act("jo", callControlBusiness.Action{Action: callControlBusiness.ActSetFeedback, Feedback: "shout"}); !errors.Is(err, callControlBusiness.ErrNoSuchFeedback) {
		t.Errorf("unknown feedback: %v", err)
	}
	if err := act("jo", callControlBusiness.Action{Action: callControlBusiness.ActSetFeedback}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Jo's feedback taken away", func() bool { return attr("jo", callControlBusiness.AttrFeedback) == "" })
}

// A guest raises and lowers their own hand with their own call token, and
// nothing more: not someone else's hand, not with a member's token, not in
// another call.
func TestCallHandsGuestActsOnlyOnThemselves(t *testing.T) {
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
	host := join(t, lk, meeting.HostToken)
	joinSettled(t, meeting.Room, people["maya"].Uid, host)
	connected(t, ctx, meeting.Room, people["maya"].Uid)

	door, err := guestBusiness.OpenDoor(ctx, meeting.GuestToken)
	if err != nil || door.Room != meeting.Room {
		t.Fatalf("the meeting's link: %+v, %v", door, err)
	}
	guestToken, room, err := guestBusiness.IssueGuestMeetingToken(ctx, door, "Gia", "")
	if err != nil || room != meeting.Room {
		t.Fatalf("guest token: %v (room %s)", err, room)
	}
	gia := join(t, lk, guestToken)
	identity, _, err := callControlBusiness.GuestOf(guestToken)
	if err != nil {
		t.Fatalf("the guest's own token wasn't read as a guest's: %v", err)
	}
	joinSettled(t, meeting.Room, identity, gia)
	connected(t, ctx, meeting.Room, identity)

	s, err := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: callStateBusiness.KindMeeting, ID: meeting.MeetingID})
	if err != nil || s == nil {
		t.Fatalf("no meeting recorded: %v", err)
	}
	raise := callControlBusiness.Action{Action: callControlBusiness.ActRaiseHand}
	if res, err := callControlBusiness.GuestActsOnThemselves(ctx, "Bearer "+guestToken, raise); err != nil || res.Affected != 1 {
		t.Fatalf("Gia raising her hand: %+v, %v", res, err)
	}
	got, _ := callStateBusiness.Get(ctx, s.ID)
	if p := got.Person(identity); p == nil || p.HandRaisedAt == nil {
		t.Fatalf("Gia's hand isn't up: %+v", p)
	}
	lowerMaya := callControlBusiness.Action{Action: callControlBusiness.ActLowerHand, Identity: people["maya"].Uid}
	if _, err := callControlBusiness.GuestActsOnThemselves(ctx, guestToken, lowerMaya); !errors.Is(err, callControlBusiness.ErrNotAllowed) {
		t.Errorf("a guest lowering the host's hand: %v, want not allowed", err)
	}
	if _, err := callControlBusiness.GuestActsOnThemselves(ctx, meeting.HostToken, raise); !errors.Is(err, callControlBusiness.ErrNotInCall) {
		t.Errorf("a member's token on the guest route: %v, want refused", err)
	}
	if _, err := callControlBusiness.GuestActsOnThemselves(ctx, "not-a-token", raise); !errors.Is(err, callControlBusiness.ErrNotInCall) {
		t.Errorf("a made-up token: %v, want refused", err)
	}
	other, err := livekitInit.LiveKitService.GenerateToken("meet-"+uuid.NewString(), "guest-"+uuid.NewString(), "Someone", false, "", livekitInit.CallPublishSources)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callControlBusiness.GuestActsOnThemselves(ctx, other, raise); !errors.Is(err, callControlBusiness.ErrNotInCall) {
		t.Errorf("a guest token for another call: %v, want refused", err)
	}
	if _, err := callControlBusiness.GuestActsOnThemselves(ctx, guestToken, callControlBusiness.Action{Action: callControlBusiness.ActLowerHand}); err != nil {
		t.Errorf("Gia lowering her own hand: %v", err)
	}
}
