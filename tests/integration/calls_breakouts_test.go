//go:build integration

package integration_test

// Calls package C, breakout rooms (meet-10, zoom-02; approved by main 26 Oct
// 06:5x): hosts and co-hosts run a call's rooms; a room's media is reached
// only through a token the server mints for a room someone may be in, and is
// thrown out of a room it may not be in; the call goes on in its rooms while
// its own room is empty, and ends with them.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/akashc777/OneCamp/tests/integration"
)

// breakoutEnv starts the stores and the call server a breakout test needs.
func breakoutEnv(t *testing.T) (context.Context, *integration.Env, *integration.DgraphEnv, *integration.LiveKitEnv) {
	t.Helper()
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	return ctx, pg, dg, integration.SetupLiveKit(t)
}

// enterCall joins members to the call in a channel's room, as LiveKit and its
// webhook would, and answers the call and who joined.
func enterCall(t *testing.T, ctx context.Context, lk *integration.LiveKitEnv, room string, people map[string]*dgraphStruct.DgraphUser, names ...string) (*callStateBusiness.Session, map[string]*joined) {
	t.Helper()
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	in := map[string]*joined{}
	t.Cleanup(func() {
		for _, j := range in {
			j.room.Disconnect()
		}
	})
	for _, name := range names {
		token, err := livekitInit.LiveKitService.GenerateToken(room, people[name].Uid, title(name), false, "", livekitInit.CallPublishSources)
		if err != nil {
			t.Fatal(err)
		}
		in[name] = join(t, lk, token)
		joinSettled(t, room, people[name].Uid, in[name])
		connected(t, ctx, room, people[name].Uid)
	}
	s, err := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room})
	if err != nil || s == nil {
		t.Fatalf("no call recorded in %s: %v", room, err)
	}
	return s, in
}

// tokenRoom is the room and identity a call token this server signed lets in.
func tokenRoom(t *testing.T, token string) (room, identity string) {
	t.Helper()
	v, err := auth.ParseAPIToken(token)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := v.Verify(livekitInit.LiveKitService.Config.ApiSecret)
	if err != nil || grants.Video == nil {
		t.Fatalf("not a call token: %v", err)
	}
	return grants.Video.Room, grants.Identity
}

// leave takes someone out of a call's room for good, as their leave's webhook
// tells the server.
func leave(t *testing.T, ctx context.Context, room, identity string, j *joined) {
	t.Helper()
	r, err := livekitInit.LiveKitService.GetRoom(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := livekitInit.LiveKitService.Participant(ctx, room, identity)
	j.room.Disconnect()
	eventually(t, identity+" to be gone from "+room, func() bool {
		p, _ := livekitInit.LiveKitService.Participant(ctx, room, identity)
		return p == nil
	})
	if info != nil {
		callControlBusiness.OnLeft(ctx, room, r.Sid, info, time.Now())
	}
}

func refusedWith(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: %v, want %v", what, err, want)
	}
}

// Lower roles first: a participant can't set, assign, spread, open, close or
// broadcast to a call's rooms, nor choose one while choosing is off; an API
// token can't, even the host's; and nobody acts on the rooms of a call they
// aren't in, nor puts someone from outside it in one.
func TestCallBreakoutsAreRunByWhoRunsTheCall(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "sam", "ana", "outsider")
	s, _ := enterCall(t, ctx, lk, room, people, "sam", "ana")
	act := func(who string, a callControlBusiness.Action) error {
		_, err := callControlBusiness.Act(ctx, people[who], s.ID, a)
		return err
	}
	set := callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsSet, Rooms: []string{"Design", "Build"}}
	assignAna := callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsAssign, Identity: people["ana"].Uid, Room: 1}
	for _, a := range []callControlBusiness.Action{
		set, assignAna,
		{Action: callControlBusiness.ActBreakoutsSpread},
		{Action: callControlBusiness.ActBreakoutsOpen},
		{Action: callControlBusiness.ActBreakoutsClose},
		{Action: callControlBusiness.ActBreakoutsBroadcast, Text: "Five minutes left"},
	} {
		refusedWith(t, "Ana, a participant, doing "+a.Action, act("ana", a), callControlBusiness.ErrNotAllowed)
	}
	if _, err := callControlBusiness.Act(helpers.WithActsThroughToken(ctx), people["sam"], s.ID, set); !errors.Is(err, callControlBusiness.ErrThroughToken) {
		t.Errorf("the host's API token setting up rooms: %v, want refused", err)
	}

	other := uuid.NewString()
	elsewhere := seedChannel(t, pg, dg, other, "lee", "stranger")
	theirs, _ := enterCall(t, ctx, lk, other, elsewhere, "lee")
	if _, err := callControlBusiness.Act(ctx, elsewhere["lee"], s.ID, set); !errors.Is(err, callControlBusiness.ErrNotInCall) {
		t.Errorf("the host of another call setting up this call's rooms: %v, want not in the call", err)
	}
	if _, err := callControlBusiness.Act(ctx, elsewhere["lee"], theirs.ID, callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsSet, Rooms: []string{"One"}}); err != nil {
		t.Fatalf("Lee sets up his own call's rooms: %v", err)
	}
	if _, err := callControlBusiness.Act(ctx, elsewhere["lee"], theirs.ID, assignAna); !errors.Is(err, callControlBusiness.ErrTargetGone) {
		t.Errorf("Lee putting Ana, from another call, in his call's room: %v, want refused", err)
	}

	if err := act("sam", set); err != nil {
		t.Fatalf("Sam sets up two rooms: %v", err)
	}
	refusedWith(t, "a room that isn't there", act("sam", callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsAssign, Identity: people["ana"].Uid, Room: 3}), callControlBusiness.ErrNoSuchRoom)
	if err := act("sam", assignAna); err != nil {
		t.Fatalf("Sam puts Ana in Design: %v", err)
	}
	if err := act("sam", callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsOpen}); err != nil {
		t.Fatalf("Sam opens the rooms: %v", err)
	}
	refusedWith(t, "setting up rooms while they are open", act("sam", set), callControlBusiness.ErrRoomsOpen)
	refusedWith(t, "Ana choosing Build while choosing is off", act("ana", callControlBusiness.Action{Action: callControlBusiness.ActBreakoutChoose, Room: 2}), callControlBusiness.ErrNotYourRoom)
	refusedWith(t, "Ana closing the rooms", act("ana", callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsClose}), callControlBusiness.ErrNotAllowed)
	if err := act("ana", callControlBusiness.Action{Action: callControlBusiness.ActBreakoutChoose, Room: 0}); err != nil {
		t.Errorf("Ana going back to the main room, which anyone may: %v", err)
	}
	if mine, _ := callStateModel.Assignments(ctx, uuid.MustParse(s.ID)); mine[people["ana"].Uid] != 0 {
		t.Errorf("Ana went back to the main room but is still in room %d", mine[people["ana"].Uid])
	}
}

// A room's token is minted only for a room its holder may be in, only while
// the rooms are open, only for someone in the call, and never through an API
// token; a token for a room someone may not be in is thrown out when it
// connects, since a room's name is never enough. Someone in a room takes the
// part they have in the call.
func TestCallBreakoutRoomTokensAreForTheRoomYouMayBeIn(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "sam", "ana", "lee", "kim", "outsider")
	s, in := enterCall(t, ctx, lk, room, people, "sam", "ana", "lee")
	token := func(who string, n int) (callControlBusiness.RoomToken, error) {
		return callControlBusiness.BreakoutToken(ctx, people[who], s.ID, n, "")
	}
	act := func(a callControlBusiness.Action) {
		t.Helper()
		if _, err := callControlBusiness.Act(ctx, people["sam"], s.ID, a); err != nil {
			t.Fatalf("Sam doing %s: %v", a.Action, err)
		}
	}
	_, err := token("ana", 1)
	refusedWith(t, "a room's token before the rooms open", err, callControlBusiness.ErrRoomsClosed)

	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsSet, Rooms: []string{"Design", "Build"}})
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsAssign, Identity: people["ana"].Uid, Room: 1})
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsAssign, Identity: people["lee"].Uid, Room: 2})
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsOpen})

	_, err = token("ana", 2)
	refusedWith(t, "Ana's token for Lee's room", err, callControlBusiness.ErrNotYourRoom)
	_, err = token("ana", 3)
	refusedWith(t, "a token for a room that isn't there", err, callControlBusiness.ErrNoSuchRoom)
	_, err = callControlBusiness.BreakoutToken(helpers.WithActsThroughToken(ctx), people["ana"], s.ID, 1, "")
	refusedWith(t, "Ana's room token through her API token", err, callControlBusiness.ErrThroughToken)
	_, err = token("kim", 1)
	refusedWith(t, "Kim, in the channel but not in the call", err, callControlBusiness.ErrNotInCall)

	design := callStateBusiness.BreakoutRoom(room, 1)
	mine, err := token("ana", 1)
	if err != nil {
		t.Fatalf("Ana's token for her own room: %v", err)
	}
	if got, who := tokenRoom(t, mine.Token); got != design || who != people["ana"].Uid || mine.Room != design || mine.Number != 1 || mine.Name != "Design" {
		t.Fatalf("Ana's room token lets %s into %s (answer %+v), want her into %s", who, got, mine, design)
	}
	roomCall, err := callStateBusiness.Get(ctx, mine.Session)
	if err != nil || roomCall == nil || roomCall.Parent != s.ID {
		t.Fatalf("Design's call %+v (%v) isn't Sam's call's room", roomCall, err)
	}
	visit, err := token("sam", 2)
	if err != nil || visit.Room != callStateBusiness.BreakoutRoom(room, 2) {
		t.Fatalf("Sam, the host, visiting Build: %+v, %v", visit, err)
	}

	ana := join(t, lk, mine.Token)
	t.Cleanup(ana.room.Disconnect)
	joinSettled(t, design, people["ana"].Uid, ana)
	connected(t, ctx, design, people["ana"].Uid)
	host := join(t, lk, visit.Token)
	t.Cleanup(host.room.Disconnect)
	joinSettled(t, visit.Room, people["sam"].Uid, host)
	connected(t, ctx, visit.Room, people["sam"].Uid)
	inDesign, _ := callStateBusiness.InRoom(ctx, design)
	if p := inDesign.Person(people["ana"].Uid); p == nil || p.Role != callStateModel.RoleParticipant {
		t.Errorf("Ana's part in her room: %+v, want a participant's", p)
	}
	inBuild, _ := callStateBusiness.InRoom(ctx, visit.Room)
	if p := inBuild.Person(people["sam"].Uid); p == nil || p.Role != callStateModel.RoleHost {
		t.Errorf("Sam's part in the room he visits: %+v, want the host's he has in the call", p)
	}

	// A token for Build, minted without the server's say, is thrown out.
	stray, err := livekitInit.LiveKitService.GenerateToken(visit.Room, people["ana"].Uid, "Ana", false, "", livekitInit.CallPublishSources)
	if err != nil {
		t.Fatal(err)
	}
	intruder := join(t, lk, stray)
	t.Cleanup(intruder.room.Disconnect)
	connected(t, ctx, visit.Room, people["ana"].Uid)
	eventually(t, "Ana's stray token to be put out of Build", func() bool {
		p, _ := livekitInit.LiveKitService.Participant(ctx, visit.Room, people["ana"].Uid)
		return p == nil
	})

	leave(t, ctx, room, people["lee"].Uid, in["lee"])
	_, err = token("lee", 2)
	refusedWith(t, "Lee, who left the call, asking for his room", err, callControlBusiness.ErrNotInCall)
}

// A guest's room token comes through their own call token, for their own
// room alone, while they are in the call; a member's token on the guest
// route is refused.
func TestCallBreakoutRoomTokensForGuests(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	people := seedChannel(t, pg, dg, uuid.NewString(), "maya")
	meeting, err := guestBusiness.CreateInstantMeeting(ctx, people["maya"], "")
	if err != nil {
		t.Fatal(err)
	}
	host := join(t, lk, meeting.HostToken)
	t.Cleanup(host.room.Disconnect)
	joinSettled(t, meeting.Room, people["maya"].Uid, host)
	connected(t, ctx, meeting.Room, people["maya"].Uid)
	door, err := guestBusiness.OpenDoor(ctx, meeting.GuestToken)
	if err != nil {
		t.Fatal(err)
	}
	guestToken, _, err := guestBusiness.IssueGuestMeetingToken(ctx, door, "Gia", "")
	if err != nil {
		t.Fatal(err)
	}
	gia := join(t, lk, guestToken)
	identity, _, err := callControlBusiness.GuestOf(guestToken)
	if err != nil {
		t.Fatal(err)
	}
	joinSettled(t, meeting.Room, identity, gia)
	connected(t, ctx, meeting.Room, identity)
	s, err := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: callStateBusiness.KindMeeting, ID: meeting.MeetingID})
	if err != nil || s == nil {
		t.Fatalf("no meeting recorded: %v", err)
	}
	for _, a := range []callControlBusiness.Action{
		{Action: callControlBusiness.ActBreakoutsSet, Rooms: []string{"Ideas", "Plans"}},
		{Action: callControlBusiness.ActBreakoutsAssign, Identity: identity, Room: 1},
		{Action: callControlBusiness.ActBreakoutsOpen},
	} {
		if _, err := callControlBusiness.Act(ctx, people["maya"], s.ID, a); err != nil {
			t.Fatalf("Maya doing %s: %v", a.Action, err)
		}
	}
	_, err = callControlBusiness.GuestBreakoutToken(ctx, "Bearer "+guestToken, 2, "")
	refusedWith(t, "Gia's token for a room she isn't in", err, callControlBusiness.ErrNotYourRoom)
	_, err = callControlBusiness.GuestBreakoutToken(ctx, meeting.HostToken, 1, "")
	refusedWith(t, "a member's token on the guest route", err, callControlBusiness.ErrNotInCall)
	got, err := callControlBusiness.GuestBreakoutToken(ctx, "Bearer "+guestToken, 1, "")
	if err != nil {
		t.Fatalf("Gia's token for her own room: %v", err)
	}
	if r, who := tokenRoom(t, got.Token); r != callStateBusiness.BreakoutRoom(meeting.Room, 1) || who != identity {
		t.Fatalf("Gia's room token lets %s into %s", who, r)
	}
	leave(t, ctx, meeting.Room, identity, gia)
	_, err = callControlBusiness.GuestBreakoutToken(ctx, "Bearer "+guestToken, 1, "")
	refusedWith(t, "Gia, who left the call, asking for her room", err, callControlBusiness.ErrNotInCall)
}

// While everyone is in the rooms the call's own room empties and LiveKit may
// close it: the call goes on, nobody is made its host for it, and its room
// made again is the same call. Everyone is called back once, however many
// ask; and ending the call ends its rooms.
func TestCallGoesOnInItsBreakoutRooms(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "sam", "ana", "lee", "outsider")
	s, in := enterCall(t, ctx, lk, room, people, "sam", "ana", "lee")
	act := func(who string, a callControlBusiness.Action) {
		t.Helper()
		if _, err := callControlBusiness.Act(ctx, people[who], s.ID, a); err != nil {
			t.Fatalf("%s doing %s: %v", who, a.Action, err)
		}
	}
	act("sam", callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsSet, Rooms: []string{"Design"}})
	act("sam", callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsSpread})
	act("sam", callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsOpen})
	design := callStateBusiness.BreakoutRoom(room, 1)
	for _, name := range []string{"ana", "lee", "sam"} {
		rt, err := callControlBusiness.BreakoutToken(ctx, people[name], s.ID, 1, "")
		if err != nil {
			t.Fatalf("%s's token for Design: %v", name, err)
		}
		j := join(t, lk, rt.Token)
		t.Cleanup(j.room.Disconnect)
		joinSettled(t, design, people[name].Uid, j)
		connected(t, ctx, design, people[name].Uid)
		leave(t, ctx, room, people[name].Uid, in[name])
	}
	r, _ := livekitInit.LiveKitService.GetRoom(ctx, room)
	if over := callControlBusiness.OnEnded(ctx, room, r.Sid, callStateModel.EndedLastLeft); over {
		t.Error("the last person leaving the call's own room for a breakout room ended the call")
	}
	liveKitBusiness.DeleteRoom(ctx, room) // LiveKit closes the empty room
	if over := callControlBusiness.OnEnded(ctx, room, r.Sid, callStateModel.EndedRoomFinished); over {
		t.Error("LiveKit closing the call's empty room ended the call")
	}
	if err := callControlBusiness.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	now, _ := callStateBusiness.Get(ctx, s.ID)
	if now == nil || !now.Live {
		t.Fatalf("the call ended while everyone was in its room: %+v", now)
	}
	if p := now.Person(people["ana"].Uid); p == nil || p.Role != callStateModel.RoleParticipant {
		t.Errorf("Ana was made to run the call when Sam went to a room: %+v", p)
	}
	if cur, _ := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room}); cur == nil || cur.ID != s.ID {
		t.Errorf("the channel's call is %+v, want Sam's call, not one of its rooms", cur)
	}

	// Sam closes the rooms from Design; everyone is called back once.
	act("sam", callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsClose})
	back := time.Now().Add(callStateModel.BreakoutCountdown + time.Second)
	first, err := callStateModel.FinishBreakouts(ctx, uuid.MustParse(s.ID), back)
	if err != nil || !first {
		t.Fatalf("the first claim on calling everyone back: %v, %v", first, err)
	}
	if again, err := callStateModel.FinishBreakouts(ctx, uuid.MustParse(s.ID), back); err != nil || again {
		t.Errorf("a second claim called everyone back again: %v, %v", again, err)
	}

	// Sam is back: the call's room, made again, is the same call.
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	token, _ := livekitInit.LiveKitService.GenerateToken(room, people["sam"].Uid, "Sam", false, "", livekitInit.CallPublishSources)
	sam := join(t, lk, token)
	t.Cleanup(sam.room.Disconnect)
	joinSettled(t, room, people["sam"].Uid, sam)
	connected(t, ctx, room, people["sam"].Uid)
	if cur, _ := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room}); cur == nil || cur.ID != s.ID {
		t.Fatalf("Sam came back to a new call %+v, want the one he left for the rooms", cur)
	}

	// Ending the call ends its rooms: Ana and Lee are still in Design.
	act("sam", callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsOpen})
	if rooms, err := callStateModel.OpenRoomsOf(ctx, uuid.MustParse(s.ID)); err != nil || len(rooms) != 1 {
		t.Fatalf("Design's call isn't going on: %d, %v", len(rooms), err)
	}
	act("sam", callControlBusiness.Action{Action: callControlBusiness.ActEnd})
	rooms, err := callStateModel.OpenRoomsOf(ctx, uuid.MustParse(s.ID))
	if err != nil || len(rooms) != 0 {
		t.Errorf("the call ended and left its rooms going: %d, %v", len(rooms), err)
	}
	eventually(t, "Design to close with the call", func() bool {
		r, err := livekitInit.LiveKitService.GetRoom(ctx, design)
		return err != nil || r == nil
	})
}

// Captions follow the call into its rooms (CR-62's condition 4 on package C).
// Transcription is the call's switch, so a flip in any of its rooms flips it in
// every one of them, a room made later takes the call's, and no room keeps a
// transcript (rooms aren't recorded). Above all, no room transcribes while the
// call's switch is off: its own room's metadata is what every browser and the
// server's transcriber in it go by, and the server's copy is what it keeps
// lines by.
func TestCallBreakoutRoomsFollowTheCallsTranscription(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	t.Setenv("TRANSCRIPTION_MODE", "frontend")
	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "sam", "ana", "lee", "outsider")
	s, in := enterCall(t, ctx, lk, room, people, "sam", "ana", "lee")
	act := func(a callControlBusiness.Action) {
		t.Helper()
		if _, err := callControlBusiness.Act(ctx, people["sam"], s.ID, a); err != nil {
			t.Fatalf("Sam doing %s: %v", a.Action, err)
		}
	}
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsSet, Rooms: []string{"Design", "Build"}})
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsAssign, Identity: people["ana"].Uid, Room: 1})
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsAssign, Identity: people["lee"].Uid, Room: 2})
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsOpen})
	enter := func(who string, n int) string {
		t.Helper()
		tok, err := callControlBusiness.BreakoutToken(ctx, people[who], s.ID, n, "")
		if err != nil {
			t.Fatalf("%s's token for room %d: %v", who, n, err)
		}
		j := join(t, lk, tok.Token)
		t.Cleanup(j.room.Disconnect)
		joinSettled(t, tok.Room, people[who].Uid, j)
		connected(t, ctx, tok.Room, people[who].Uid)
		leave(t, ctx, room, people[who].Uid, in[who])
		return tok.Room
	}
	// What a room says (its metadata, as its browsers read it) and what the
	// server keeps lines by must agree; the switch is that.
	switchIn := func(r string) liveKitBusiness.TranscriptionSwitch {
		t.Helper()
		lkRoom, err := livekitInit.LiveKitService.GetRoom(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		kept, _ := liveKitBusiness.CallSwitches(ctx, r, lkRoom.Sid)
		view, err := liveKitBusiness.ReadCallView(ctx, r, false)
		if err != nil {
			t.Fatal(err)
		}
		if view.Transcription.On != kept.On {
			t.Fatalf("%s says transcription is %v, the server keeps lines while %v", r, view.Transcription.On, kept.On)
		}
		return kept
	}

	design := enter("ana", 1)
	if _, err := liveKitBusiness.SetTranscription(ctx, design, people["ana"].Uid, "Ana", true); err != nil {
		t.Fatalf("Ana turning transcription on in Design: %v", err)
	}
	if sw := switchIn(room); !sw.On || sw.By != "Ana" || !sw.Kept {
		t.Errorf("the call's own room after Ana turned transcription on in Design: %+v, want on, by Ana, kept", sw)
	}
	if sw := switchIn(design); !sw.On || sw.Kept {
		t.Errorf("Design: %+v, want on, and kept nowhere", sw)
	}

	build := enter("lee", 2)
	if sw := switchIn(build); !sw.On || sw.By != "Ana" || sw.Kept {
		t.Errorf("Build, made after: %+v, want the call's switch, kept nowhere", sw)
	}

	if _, err := liveKitBusiness.SetTranscription(ctx, room, people["sam"].Uid, "Sam", false); err != nil {
		t.Fatalf("Sam turning transcription off in the call's own room: %v", err)
	}
	for _, r := range []string{room, design, build} {
		if sw := switchIn(r); sw.On {
			t.Errorf("%s still transcribes after Sam turned the call's transcription off", r)
		}
	}
}

// A hold changed while the call's rooms are open reaches them (CR-62's SF2 on
// package C): a room's call took what its call held as it opened, and then
// nothing, so a microphone held for everyone stayed open in every room. Now
// the room's record, what each person in it may publish, and what its screens
// read follow the call's hold, on and off again.
func TestABreakoutRoomHoldsWhatItsCallHoldsWhenItChanges(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "sam", "ana", "outsider") // the last one seeded is no member
	s, in := enterCall(t, ctx, lk, room, people, "sam", "ana")
	act := func(a callControlBusiness.Action) {
		t.Helper()
		if _, err := callControlBusiness.Act(ctx, people["sam"], s.ID, a); err != nil {
			t.Fatalf("Sam doing %s: %v", a.Action, err)
		}
	}
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsSet, Rooms: []string{"Design"}})
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsAssign, Identity: people["ana"].Uid, Room: 1})
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsOpen})
	design := callStateBusiness.BreakoutRoom(room, 1)
	rt, err := callControlBusiness.BreakoutToken(ctx, people["ana"], s.ID, 1, "")
	if err != nil {
		t.Fatalf("Ana's token for Design: %v", err)
	}
	j := join(t, lk, rt.Token)
	t.Cleanup(j.room.Disconnect)
	joinSettled(t, design, people["ana"].Uid, j)
	connected(t, ctx, design, people["ana"].Uid)
	leave(t, ctx, room, people["ana"].Uid, in["ana"])

	mayTalk := func() bool {
		p, _ := livekitInit.LiveKitService.Participant(ctx, design, people["ana"].Uid)
		return p != nil && p.Permission != nil && slices.Contains(p.Permission.CanPublishSources, livekit.TrackSource_MICROPHONE)
	}
	roomHolds := func() (record []string, meta string) {
		r, err := callStateModel.OpenRoomsOf(ctx, uuid.MustParse(s.ID))
		if err != nil || len(r) != 1 {
			t.Fatalf("Design's call: %d, %v", len(r), err)
		}
		lkr, err := livekitInit.LiveKitService.GetRoom(ctx, design)
		if err != nil || lkr == nil {
			t.Fatalf("Design's room: %v", err)
		}
		return r[0].Held, lkr.Metadata
	}
	if !mayTalk() {
		t.Fatal("Ana may not talk in Design before any hold")
	}

	act(callControlBusiness.Action{Action: callControlBusiness.ActHold, What: "microphone", On: true})
	if record, meta := roomHolds(); !slices.Contains(record, "microphone") || !strings.Contains(meta, `"held":["microphone"]`) {
		t.Errorf("microphones held for the call: Design holds %v, its screens read %q", record, meta)
	}
	if mayTalk() {
		t.Error("microphones held for the call: Ana may still talk in Design")
	}

	act(callControlBusiness.Action{Action: callControlBusiness.ActHold, What: "microphone", On: false})
	if record, meta := roomHolds(); slices.Contains(record, "microphone") || strings.Contains(meta, `"held"`) {
		t.Errorf("microphones let again: Design holds %v, its screens read %q", record, meta)
	}
	eventually(t, "Ana to be let talk in Design again", mayTalk)
}

// Time in a call's rooms is time in the call (CR-62's SF3 on package C): the
// call's attendance counted only its own room, so someone who spent the call
// in a breakout room read as having left it early. A person's time in the call
// now adds what they spent in its rooms.
func TestTimeInABreakoutRoomCountsAsTimeInTheCall(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "sam", "ana", "outsider") // the last one seeded is no member
	s, in := enterCall(t, ctx, lk, room, people, "sam", "ana")
	for _, a := range []callControlBusiness.Action{
		{Action: callControlBusiness.ActBreakoutsSet, Rooms: []string{"Design"}},
		{Action: callControlBusiness.ActBreakoutsAssign, Identity: people["ana"].Uid, Room: 1},
		{Action: callControlBusiness.ActBreakoutsOpen},
	} {
		if _, err := callControlBusiness.Act(ctx, people["sam"], s.ID, a); err != nil {
			t.Fatalf("Sam doing %s: %v", a.Action, err)
		}
	}
	design := callStateBusiness.BreakoutRoom(room, 1)
	rt, err := callControlBusiness.BreakoutToken(ctx, people["ana"], s.ID, 1, "")
	if err != nil {
		t.Fatalf("Ana's token for Design: %v", err)
	}
	j := join(t, lk, rt.Token)
	t.Cleanup(j.room.Disconnect)
	joinSettled(t, design, people["ana"].Uid, j)
	connected(t, ctx, design, people["ana"].Uid)
	leave(t, ctx, room, people["ana"].Uid, in["ana"])
	time.Sleep(2500 * time.Millisecond) // Ana's time in Design
	leave(t, ctx, design, people["ana"].Uid, j)

	secondsIn := func(session uuid.UUID) int {
		t.Helper()
		var n int
		if err := pg.PG.QueryRow(`SELECT COALESCE(SUM(seconds_in), 0) FROM call_attendance WHERE session_id = $1 AND identity = $2`, session, people["ana"].Uid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var inDesign int
	if err := pg.PG.QueryRow(`SELECT COALESCE(SUM(a.seconds_in), 0) FROM call_attendance a JOIN call_sessions r ON r.id = a.session_id
		WHERE r.parent_session_id = $1 AND a.identity = $2`, uuid.MustParse(s.ID), people["ana"].Uid).Scan(&inDesign); err != nil {
		t.Fatal(err)
	}
	if inDesign < 2 {
		t.Fatalf("Ana's time in Design as recorded: %ds, want at least 2", inDesign)
	}
	call, err := callStateBusiness.Get(ctx, s.ID)
	if err != nil || call == nil {
		t.Fatalf("the call: %v", err)
	}
	ana := call.Person(people["ana"].Uid)
	if ana == nil {
		t.Fatal("Ana isn't in the call's attendance")
	}
	if want := secondsIn(uuid.MustParse(s.ID)) + inDesign; ana.SecondsIn != want {
		t.Errorf("Ana's time in the call: %ds, want %ds (its own room's and Design's %ds)", ana.SecondsIn, want, inDesign)
	}
	// The calls going on read every call's time in its rooms at once (CR-67's
	// SF on package C) and say the same as the call read alone.
	open, err := callStateBusiness.OpenCalls(ctx, 10000)
	if err != nil {
		t.Fatalf("the calls going on: %v", err)
	}
	listed := false
	for _, c := range open {
		if c.ID != s.ID {
			continue
		}
		listed = true
		if p := c.Person(people["ana"].Uid); p == nil || p.SecondsIn != ana.SecondsIn {
			t.Errorf("Ana in the calls going on: %+v, want %ds as the call read alone", p, ana.SecondsIn)
		}
	}
	if !listed {
		t.Error("the call isn't among the calls going on")
	}
}
