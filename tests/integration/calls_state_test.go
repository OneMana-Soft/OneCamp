//go:build integration
// +build integration

package integration_test

// Call State and Call Control (the calls programme's CALLS-1) against real
// Postgres 12, Dgraph, Redis and the call server the customer stack pins.
// LiveKit's webhook isn't pointed at a test, so a test tells the server what
// the webhook would (connected), as HandleWebhook does.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestCallState|TestCallControl|TestTheCallRead|TestCallAHost|TestCallWhoHosts' -v

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// connected tells the server that someone connected to a call, as LiveKit's
// participant_joined webhook does, and answers the room's LiveKit id.
func connected(t *testing.T, ctx context.Context, room, identity string) string {
	t.Helper()
	r, err := livekitInit.LiveKitService.GetRoom(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	var p *livekit.ParticipantInfo
	eventually(t, identity+" to be in "+room, func() bool {
		p, err = livekitInit.LiveKitService.Participant(ctx, room, identity)
		return err == nil && p != nil
	})
	callControlBusiness.OnJoined(ctx, room, r.Sid, p, time.Now())
	return r.Sid
}

// callEventLog records what was emitted, in place of the bus.
type callEventLog struct {
	mu     sync.Mutex
	events []resourceEventBusiness.Event
}

func (l *callEventLog) Emit(_ context.Context, e resourceEventBusiness.Event) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

// of answers the events of a kind.
func (l *callEventLog) of(kind resourceEventBusiness.Kind) []resourceEventBusiness.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []resourceEventBusiness.Event
	for _, e := range l.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func recordEvents(t *testing.T) *callEventLog {
	t.Helper()
	l := &callEventLog{}
	old := resourceEventBusiness.Bus
	resourceEventBusiness.Bus = l
	t.Cleanup(func() { resourceEventBusiness.Bus = old })
	return l
}

// told answers the newest packet of a kind j received on the call's control
// topic, and who sent it ("" is the server).
func told(j *joined, kind string) (map[string]any, string, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i := len(j.packets) - 1; i >= 0; i-- {
		p := j.packets[i]
		var m map[string]any
		if p.Topic == "oc.call" && json.Unmarshal(p.Payload, &m) == nil && m["t"] == kind {
			return m, j.packetSender[i], true
		}
	}
	return nil, "", false
}

// Call State's migration goes down and up again on Postgres 12, and its one
// rule in the schema holds: one call going on per room. A call's polls and
// questions (call_activities), the boards on its stage (call_board) and its
// breakout rooms (call_breakouts) hang off call_sessions, so they come off
// first and go back on last, the order a migrate down and up takes.
func TestCallStateMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	exists := func(table string) bool {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_name = $1`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	tables := []string{"call_sessions", "call_attendance", "call_recordings", "call_breakouts", "call_breakout_assignments"}
	for _, name := range []string{"call_breakouts", "call_board", "call_activities", "call_sessions"} {
		if _, err := env.PG.Exec(migrationSQL(t, name, "down")); err != nil {
			t.Fatalf("down %s: %v", name, err)
		}
	}
	for _, table := range tables {
		if exists(table) {
			t.Errorf("%s is still there after down", table)
		}
	}
	for _, name := range []string{"call_sessions", "call_activities", "call_board", "call_breakouts"} {
		if _, err := env.PG.Exec(migrationSQL(t, name, "up")); err != nil {
			t.Fatalf("up %s again: %v", name, err)
		}
	}
	for _, table := range tables {
		if !exists(table) {
			t.Errorf("%s is missing after up", table)
		}
	}
	open := func() error {
		_, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at) VALUES ($1, 'room-1', 'huddle', 'channel', 'room-1', NOW())`, uuid.New())
		return err
	}
	if err := open(); err != nil {
		t.Fatal(err)
	}
	if err := open(); err == nil {
		t.Error("a second call going on in one room was stored")
	}
	if _, err := env.PG.Exec(`UPDATE call_sessions SET ended_at = NOW() WHERE room = 'room-1'`); err != nil {
		t.Fatal(err)
	}
	if err := open(); err != nil {
		t.Errorf("a room's next call, after the last ended: %v", err)
	}
}

// One call is one session from its first join to its last leave: repeated and
// stale webhooks change nothing, a rejoin is the same person back, their time
// in it adds up, and every event is told once. A room LiveKit closed and made
// again is a new call, and the old one ends.
func TestCallStateOneSessionFromFirstJoinToLastLeave(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	log := recordEvents(t)
	room := uuid.NewString()
	ref := callStateBusiness.Ref{Kind: "channel", ID: room}
	role := func(_ context.Context, _ callStateBusiness.Ref, e callStateBusiness.JoinEvent, starter bool) string {
		switch {
		case e.UserUUID == nil:
			return callStateModel.RoleGuest
		case starter:
			return callStateModel.RoleHost
		}
		return callStateModel.RoleParticipant
	}
	maya, sam := uuid.New(), uuid.New()
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	join := func(identity string, user *uuid.UUID, sid string, at time.Time) (*callStateBusiness.Session, *callStateBusiness.Person) {
		t.Helper()
		s, p, err := callStateBusiness.Joined(ctx, callStateBusiness.JoinEvent{Room: room, RoomSID: sid, Identity: identity, Name: identity, UserUUID: user, At: at}, role)
		if err != nil {
			t.Fatal(err)
		}
		return s, p
	}
	leave := func(identity string, user *uuid.UUID, at time.Time) bool {
		t.Helper()
		_, changed, err := callStateBusiness.Left(ctx, callStateBusiness.JoinEvent{Room: room, RoomSID: "RM_1", Identity: identity, Name: identity, UserUUID: user, At: at})
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}

	first, p := join("0x1", &maya, "RM_1", t0)
	if !first.Live || p == nil || p.Role != callStateModel.RoleHost {
		t.Fatalf("the first join: %+v %+v", first, p)
	}
	join("0x1", &maya, "RM_1", t0) // LiveKit sent it twice
	if _, p := join("0x2", &sam, "RM_1", t0.Add(10*time.Second)); p.Role != callStateModel.RoleParticipant {
		t.Errorf("the second in takes %q", p.Role)
	}
	if _, p := join("guest-a-b", nil, "RM_1", t0.Add(20*time.Second)); p.Role != callStateModel.RoleGuest || !p.Guest {
		t.Errorf("a guest takes %+v", p)
	}
	if !leave("0x2", &sam, t0.Add(70*time.Second)) {
		t.Fatal("Sam's leave changed nothing")
	}
	if leave("0x2", &sam, t0.Add(70*time.Second)) {
		t.Error("a repeated leave changed something")
	}
	join("0x2", &sam, "RM_1", t0.Add(60*time.Second)) // a join older than Sam's leave, late
	cur, err := callStateBusiness.Current(ctx, ref)
	if err != nil || cur == nil || cur.ID != first.ID {
		t.Fatalf("the call going on: %+v %v, want %s", cur, err, first.ID)
	}
	if s := cur.Person("0x2"); s == nil || s.In || s.SecondsIn != 60 {
		t.Fatalf("Sam after leaving: %+v", s)
	}
	join("0x2", &sam, "RM_1", t0.Add(100*time.Second)) // back
	if cur, _ = callStateBusiness.Current(ctx, ref); !cur.Person("0x2").In || len(cur.People) != 3 {
		t.Fatalf("Sam back: %+v", cur.People)
	}

	ended, did, err := callStateBusiness.End(ctx, room, "RM_1", callStateModel.EndedLastLeft)
	if err != nil || !did || ended.Live || ended.EndedReason != callStateModel.EndedLastLeft || len(ended.InCall()) != 0 {
		t.Fatalf("the end: %+v %v %v", ended, did, err)
	}
	if _, again, _ := callStateBusiness.End(ctx, room, "RM_1", callStateModel.EndedRoomFinished); again {
		t.Error("the call ended twice")
	}
	if cur, _ = callStateBusiness.Current(ctx, ref); cur != nil {
		t.Errorf("a call is still going on after its end: %+v", cur)
	}

	if n := len(log.of(resourceEventBusiness.CallStarted)); n != 1 {
		t.Errorf("call.started was told %d times", n)
	}
	joins := log.of(resourceEventBusiness.CallJoined)
	if len(joins) != 4 || joins[3].Data["identity"] != "0x2" || joins[3].Data["rejoin"] != "true" || joins[0].Data["rejoin"] != "false" {
		t.Errorf("call.joined: %+v", joins)
	}
	if n := len(log.of(resourceEventBusiness.CallLeft)); n != 1 {
		t.Errorf("call.left was told %d times", n)
	}
	endedEvents := log.of(resourceEventBusiness.CallEnded)
	if len(endedEvents) != 1 || endedEvents[0].ResourceKind != "channel" || endedEvents[0].ResourceUUID != room || endedEvents[0].Data["session"] != first.ID || endedEvents[0].Data["reason"] != "last_left" {
		t.Errorf("call.ended: %+v", endedEvents)
	}

	second, _ := join("0x1", &maya, "RM_2", t0.Add(2*time.Hour))
	if second.ID == first.ID {
		t.Fatal("the room's next call is the same session")
	}
	third, _ := join("0x1", &maya, "RM_3", t0.Add(3*time.Hour)) // LiveKit made the room again; its end never came
	if third.ID == second.ID {
		t.Fatal("a call in a room LiveKit made again is the old session")
	}
	if old, _ := callStateBusiness.Get(ctx, second.ID); old == nil || old.Live || old.EndedReason != callStateModel.EndedReconciled {
		t.Errorf("the stale call: %+v", old)
	}
}

// title is a name as it is shown: "maya" is "Maya". Pure.
func title(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// micOnServer reports whether the call server lists identity's microphone,
// on: what a host's mute acts on.
func micOnServer(ctx context.Context, room, identity string) bool {
	p, _ := livekitInit.LiveKitService.Participant(ctx, room, identity)
	return slices.ContainsFunc(p.GetTracks(), func(tr *livekit.TrackInfo) bool {
		return tr.Source == livekit.TrackSource_MICROPHONE && !tr.Muted
	})
}

// seedChannel makes a channel with these people (the first its moderator),
// each a user in the graph and in Postgres, and answers their graph uids by
// name. The last of them is left out of the channel's members: the outsider
// a test keeps out, so name one ("outsider") after everyone who joins.
func seedChannel(t *testing.T, pg *integration.Env, dg *integration.DgraphEnv, channelUUID string, names ...string) map[string]*dgraphStruct.DgraphUser {
	t.Helper()
	var nodes []map[string]any
	var members []map[string]any
	ids := map[string]uuid.UUID{}
	for _, n := range names {
		ids[n] = uuid.New()
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": ids[n].String(), "user_name": n})
		members = append(members, map[string]any{"uid": "_:" + n})
		if _, err := pg.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`, ids[n], n+"@example.test", n, title(n)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pg.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private, created_by) VALUES ($1, $2, false, $3)`, channelUUID, "launch-"+channelUUID[:8], ids[names[0]]); err != nil {
		t.Fatal(err)
	}
	nodes = append(nodes, map[string]any{"uid": "_:c", "dgraph.type": "Channel", "ch_uuid": channelUUID, "ch_name": "launch",
		// Live, as every channel the app writes is: a zero deletion time, never
		// none (the reply route reads it).
		"ch_deleted_at": "0001-01-01T00:00:00Z",
		"ch_members":    members[:len(members)-1], "ch_moderators": []map[string]any{{"uid": "_:" + names[0]}}})
	uids := dg.Mutate(t, nodes)
	out := map[string]*dgraphStruct.DgraphUser{}
	for _, n := range names {
		out[n] = &dgraphStruct.DgraphUser{Uid: uids[n], Uuid: ids[n].String(), UserName: n}
	}
	return out
}

// The people running a call run it, on a real call server: whoever starts a
// channel's call and its moderators host it; a host mutes one person or
// everyone (never the hosts), asks people to unmute (and never unmutes them),
// names co-hosts, locks the call (nobody new comes in, an older token is put
// out on arrival), removes someone (who can't come back) and ends it for
// everyone. A participant runs nothing, a co-host doesn't end the call or
// touch a host, and nobody runs a call through an API token. Each person's
// part reaches every screen as attributes only the server sets.
func TestCallControlLetsHostsRunACall(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	lk := integration.SetupLiveKit(t)
	log := recordEvents(t)

	room := uuid.NewString()
	// Maya moderates the channel; Sam, Ana, Jo and Lee are its members. The
	// last name seeded is outside the channel.
	people := seedChannel(t, pg, dg, room, "maya", "sam", "ana", "jo", "lee", "outsider")
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	in := map[string]*joined{}
	enter := func(name string) *joined {
		t.Helper()
		token, err := livekitInit.LiveKitService.GenerateToken(room, people[name].Uid, title(name), false, "", livekitInit.CallPublishSources)
		if err != nil {
			t.Fatal(err)
		}
		j := join(t, lk, token)
		joinSettled(t, room, people[name].Uid, j)
		if err := publishTrack(j.room, livekit.TrackSource_MICROPHONE); err != nil {
			t.Fatal(err)
		}
		connected(t, ctx, room, people[name].Uid)
		// The call server lists a track once its first packets arrive, a
		// moment after the client has published it; a mute before then finds
		// no microphone to turn off. From 20 Oct the hosts acted inside that
		// moment every run ("Maya mutes Ana: {Affected:0}").
		eventually(t, name+"'s microphone to reach the call server", func() bool { return micOnServer(ctx, room, people[name].Uid) })
		in[name] = j
		return j
	}
	enter("sam") // starts the call
	enter("maya")
	ana := enter("ana")
	jo := enter("jo")

	s, err := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room})
	if err != nil || s == nil {
		t.Fatalf("no call recorded: %v", err)
	}
	for name, want := range map[string]string{"sam": "host", "maya": "host", "ana": "participant", "jo": "participant"} {
		if p := s.Person(people[name].Uid); p == nil || p.Role != want {
			t.Errorf("%s's part: %+v, want %s", name, p, want)
		}
	}
	attrs := func(name string) map[string]string {
		p, _ := livekitInit.LiveKitService.Participant(ctx, room, people[name].Uid)
		if p == nil {
			return nil
		}
		return p.Attributes
	}
	eventually(t, "the room to be told each part", func() bool {
		return attrs("maya")[callControlBusiness.AttrRole] == "host" && strings.Contains(attrs("maya")[callControlBusiness.AttrCan], "end") &&
			attrs("ana")[callControlBusiness.AttrRole] == "participant" && attrs("ana")[callControlBusiness.AttrCan] == ""
	})

	act := func(by string, a callControlBusiness.Action) (callControlBusiness.Result, error) {
		return callControlBusiness.Act(ctx, people[by], s.ID, a)
	}
	micMuted := func(name string) bool {
		p, _ := livekitInit.LiveKitService.Participant(ctx, room, people[name].Uid)
		for _, tr := range p.GetTracks() {
			if tr.Source == livekit.TrackSource_MICROPHONE {
				return tr.Muted
			}
		}
		return false
	}

	if _, err := act("ana", callControlBusiness.Action{Action: callControlBusiness.ActMute, Identity: people["jo"].Uid}); !errors.Is(err, callControlBusiness.ErrNotAllowed) {
		t.Errorf("a participant muted someone: %v", err)
	}
	if _, err := callControlBusiness.Act(helpers.WithActsThroughToken(ctx), people["maya"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActMuteAll}); !errors.Is(err, callControlBusiness.ErrThroughToken) {
		t.Errorf("a host's API token muted everyone: %v", err)
	}

	if r, err := act("maya", callControlBusiness.Action{Action: callControlBusiness.ActMute, Identity: people["ana"].Uid}); err != nil || r.Affected != 1 {
		t.Fatalf("Maya mutes Ana: %+v %v", r, err)
	}
	eventually(t, "Ana's microphone to be off", func() bool { return micMuted("ana") })
	eventually(t, "Ana to be told who muted her", func() bool {
		m, from, ok := told(ana, "muted")
		return ok && from == "" && m["by"] == "Maya" && m["source"] == "microphone"
	})
	if _, err := act("maya", callControlBusiness.Action{Action: callControlBusiness.ActAskUnmute, Identity: people["ana"].Uid}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Ana to be asked to unmute", func() bool { m, _, ok := told(ana, "ask_unmute"); return ok && m["by"] == "Maya" })
	if !micMuted("ana") {
		t.Error("asking to unmute turned Ana's microphone on: only she may")
	}

	if r, err := act("maya", callControlBusiness.Action{Action: callControlBusiness.ActMuteAll}); err != nil || r.Affected != 1 {
		t.Fatalf("Maya mutes everyone: %+v %v (want Jo alone: Ana is already muted, Sam hosts)", r, err)
	}
	eventually(t, "Jo's microphone to be off", func() bool { return micMuted("jo") })
	if micMuted("sam") {
		t.Error("mute all muted a host")
	}

	if _, err := act("maya", callControlBusiness.Action{Action: callControlBusiness.ActMakeCohost, Identity: people["ana"].Uid}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Ana to be a co-host on every screen", func() bool {
		return attrs("ana")[callControlBusiness.AttrRole] == "cohost" && strings.Contains(attrs("ana")[callControlBusiness.AttrCan], "remove")
	})
	if m, _, ok := told(ana, "role"); !ok || m["role"] != "cohost" {
		t.Errorf("Ana was told %v", m)
	}
	if _, err := act("ana", callControlBusiness.Action{Action: callControlBusiness.ActEnd}); !errors.Is(err, callControlBusiness.ErrNotAllowed) {
		t.Errorf("a co-host ended the call: %v", err)
	}
	if _, err := act("ana", callControlBusiness.Action{Action: callControlBusiness.ActRemove, Identity: people["maya"].Uid}); !errors.Is(err, callControlBusiness.ErrAboveYou) {
		t.Errorf("a co-host put a host out: %v", err)
	}

	// Lee's token is from before the lock; Lee arrives after it.
	leeToken, err := livekitInit.LiveKitService.GenerateToken(room, people["lee"].Uid, "Lee", false, "", livekitInit.CallPublishSources)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := act("ana", callControlBusiness.Action{Action: callControlBusiness.ActLock}); err != nil {
		t.Fatal(err)
	}
	if r, _ := livekitInit.LiveKitService.GetRoom(ctx, room); !strings.Contains(r.GetMetadata(), `"locked"`) {
		t.Errorf("the room doesn't say it is locked: %s", r.GetMetadata())
	}
	if _, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, people["lee"], false, ""); !errors.Is(err, liveKitBusiness.ErrCallLocked) {
		t.Errorf("a newcomer minted a token into a locked call: %v", err)
	}
	if err := liveKitBusiness.MayEnter(ctx, room, people["jo"], false); err != nil {
		t.Errorf("someone in the call before the lock can't come back: %v", err)
	}
	if err := liveKitBusiness.MayEnter(ctx, room, people["maya"], false); err != nil {
		t.Errorf("a host can't come back to a locked call: %v", err)
	}
	lee := join(t, lk, leeToken)
	joinSettled(t, room, people["lee"].Uid, lee)
	connected(t, ctx, room, people["lee"].Uid)
	eventually(t, "Lee to be put out of the locked call", func() bool { return lee.disconnected.Load() != nil })
	if r := toldRemoved(lee); r != "locked" {
		t.Errorf("Lee was told %q", r)
	}
	if _, err := act("maya", callControlBusiness.Action{Action: callControlBusiness.ActUnlock}); err != nil {
		t.Fatal(err)
	}
	if err := liveKitBusiness.MayEnter(ctx, room, people["lee"], false); err != nil {
		t.Errorf("after the unlock, Lee still can't come in: %v", err)
	}

	if _, err := act("maya", callControlBusiness.Action{Action: callControlBusiness.ActRemove, Identity: people["jo"].Uid}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Jo to be put out", func() bool { return jo.disconnected.Load() != nil })
	if _, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, people["jo"], false, ""); !errors.Is(err, liveKitBusiness.ErrRemovedFromCall) {
		t.Errorf("Jo minted a token back in: %v", err)
	}
	if n := len(log.of(resourceEventBusiness.CallParticipantRemoved)); n != 1 {
		t.Errorf("call.participant.removed was told %d times", n)
	}
	if n := len(log.of(resourceEventBusiness.CallLocked)) + len(log.of(resourceEventBusiness.CallUnlocked)); n != 2 {
		t.Errorf("the lock and unlock were told %d times", n)
	}
	if n := len(log.of(resourceEventBusiness.CallRoleChanged)); n != 1 {
		t.Errorf("call.role_changed was told %d times", n)
	}

	if _, err := act("maya", callControlBusiness.Action{Action: callControlBusiness.ActEnd}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sam", "maya", "ana"} {
		j := in[name]
		eventually(t, name+" to be out of the ended call", func() bool { return j.disconnected.Load() != nil })
	}
	if m, from, ok := told(ana, "ended"); !ok || from != "" || m["by"] != "Maya" {
		t.Errorf("Ana was told %v (from %q)", m, from)
	}
	over, _ := callStateBusiness.Get(ctx, s.ID)
	if over == nil || over.Live || over.EndedReason != callStateModel.EndedByHost {
		t.Errorf("the call after End for everyone: %+v", over)
	}
	if _, err := act("maya", callControlBusiness.Action{Action: callControlBusiness.ActMuteAll}); !errors.Is(err, callControlBusiness.ErrCallOver) {
		t.Errorf("acting on an ended call: %v", err)
	}
}

// When the last host and co-host leave a call people are still in, the
// member who has been in it longest hosts it, and is told; a guest never does.
func TestCallControlHandsACallOnWhenItsHostsLeave(t *testing.T) {
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
	people := seedChannel(t, pg, dg, room, "sam", "ana", "jo", "outsider")
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	joinAs := func(name string) *joined {
		token, _ := livekitInit.LiveKitService.GenerateToken(room, people[name].Uid, title(name), false, "", livekitInit.CallPublishSources)
		j := join(t, lk, token)
		joinSettled(t, room, people[name].Uid, j)
		connected(t, ctx, room, people[name].Uid)
		return j
	}
	sam := joinAs("sam") // starts it, so hosts it
	ana := joinAs("ana")
	joinAs("jo")

	r, _ := livekitInit.LiveKitService.GetRoom(ctx, room)
	samInfo, _ := livekitInit.LiveKitService.Participant(ctx, room, people["sam"].Uid)
	sam.room.Disconnect()
	callControlBusiness.OnLeft(ctx, room, r.Sid, samInfo, time.Now())

	s, _ := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room})
	if p := s.Person(people["ana"].Uid); p == nil || p.Role != callStateModel.RoleHost {
		t.Fatalf("after the host left, Ana (in longest) is %+v", p)
	}
	if p := s.Person(people["jo"].Uid); p.Role != callStateModel.RoleParticipant {
		t.Errorf("Jo is %s", p.Role)
	}
	eventually(t, "Ana to be told she hosts it now", func() bool {
		m, _, ok := told(ana, "role")
		return ok && m["role"] == "host" && m["reason"] == "handed_on"
	})
}

// The sweep makes the record what LiveKit says: a call whose start was never
// heard is recorded with its people, someone whose leave was lost is out, and
// a call whose room went is over.
func TestCallStateReconcileHealsLostWebhooks(t *testing.T) {
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
	people := seedChannel(t, pg, dg, room, "sam", "ana", "outsider")
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	var js []*joined
	for _, name := range []string{"sam", "ana"} {
		token, _ := livekitInit.LiveKitService.GenerateToken(room, people[name].Uid, title(name), false, "", livekitInit.CallPublishSources)
		j := join(t, lk, token)
		joinSettled(t, room, people[name].Uid, j)
		js = append(js, j)
	}
	ref := callStateBusiness.Ref{Kind: "channel", ID: room}
	if s, _ := callStateBusiness.Current(ctx, ref); s != nil {
		t.Fatalf("a call is recorded before anything said so: %+v", s)
	}
	// LiveKit counts a room's people every 5 s (its roomUpdateInterval), and
	// the sweep reads the count first: a call that began in the last seconds
	// is the next pass's.
	eventually(t, "LiveKit to count both people in the room", func() bool {
		rooms, _ := livekitInit.LiveKitService.ListAllRooms(ctx)
		for _, r := range rooms {
			if r != nil && r.Name == room {
				return r.NumParticipants >= 2
			}
		}
		return false
	})
	if err := callControlBusiness.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s, _ := callStateBusiness.Current(ctx, ref)
	if s == nil || len(s.InCall()) != 2 {
		t.Fatalf("the sweep didn't record the call LiveKit has: %+v", s)
	}

	js[1].room.Disconnect() // Ana leaves; her webhook is lost
	eventually(t, "Ana to be gone from LiveKit", func() bool {
		p, _ := livekitInit.LiveKitService.Participant(ctx, room, people["ana"].Uid)
		return p == nil
	})
	if err := callControlBusiness.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if s, _ = callStateBusiness.Current(ctx, ref); s.Person(people["ana"].Uid).In {
		t.Error("Ana is still in the call after the sweep")
	}

	liveKitBusiness.DeleteRoom(ctx, room) // the room goes; room_finished is lost
	if err := callControlBusiness.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if s, _ = callStateBusiness.Current(ctx, ref); s != nil {
		t.Errorf("the call is still going on after its room went: %+v", s)
	}
}

// callRead calls one of the calls routes as a member.
func callRead(t *testing.T, h http.HandlerFunc, method, target string, who *dgraphStruct.DgraphUser, body string, sessionID string) (int, map[string]json.RawMessage) {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	info := userModels.UserInfo{UserDgraphInfo: *who}
	if id, err := uuid.Parse(who.Uuid); err == nil {
		info.UserPostgresInfo.Id = id
	}
	ctx := context.WithValue(r.Context(), helpers.UserInfoContextKey, info)
	rc := chi.NewRouteContext()
	rc.URLParams.Add("session_id", sessionID)
	r = r.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rc))
	w := httptest.NewRecorder()
	h(w, r)
	var out map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// A call is read by the people of its conversation only, says who is in it,
// what the reader may do and, while it is locked, that a newcomer can't come
// in; someone outside the channel can't see it is there.
func TestTheCallReadShowsACallToItsPeopleOnly(t *testing.T) {
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
	people := seedChannel(t, pg, dg, room, "maya", "sam", "outsider")
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	token, _ := livekitInit.LiveKitService.GenerateToken(room, people["maya"].Uid, "Maya", false, "", livekitInit.CallPublishSources)
	maya := join(t, lk, token)
	joinSettled(t, room, people["maya"].Uid, maya)
	connected(t, ctx, room, people["maya"].Uid)

	target := "/calls/current?kind=channel&id=" + room
	code, body := callRead(t, callsController.CurrentCall, http.MethodGet, target, people["maya"], "", "")
	var got struct {
		Live    bool                       `json:"live"`
		Session *callStateBusiness.Session `json:"session"`
		You     struct {
			Role    string   `json:"role"`
			Can     []string `json:"can"`
			Refusal *struct {
				Code string `json:"code"`
			} `json:"refusal"`
		} `json:"you"`
	}
	if err := json.Unmarshal(body["data"], &got); code != http.StatusOK || err != nil || !got.Live || got.Session == nil || got.You.Role != "host" || !slices.Contains(got.You.Can, "lock") {
		t.Fatalf("Maya reads her call: %d %s %v", code, body["data"], err)
	}
	if code, _ := callRead(t, callsController.CurrentCall, http.MethodGet, target, people["outsider"], "", ""); code != http.StatusNotFound {
		t.Errorf("someone outside the channel read its call: %d", code)
	}
	if code, _ := callRead(t, callsController.CurrentCall, http.MethodGet, "/calls/current?kind=group&id="+room, people["maya"], "", ""); code != http.StatusBadRequest {
		t.Errorf("a channel's room named as a group's: %d", code)
	}

	code, body = callRead(t, callsController.ActInCall, http.MethodPost, "/calls/"+got.Session.ID+"/act", people["maya"], `{"action":"lock"}`, got.Session.ID)
	if code != http.StatusOK {
		t.Fatalf("Maya locks her call: %d %s", code, body["msg"])
	}
	code, body = callRead(t, callsController.CurrentCall, http.MethodGet, target, people["sam"], "", "")
	if err := json.Unmarshal(body["data"], &got); code != http.StatusOK || err != nil || got.You.Refusal == nil || got.You.Refusal.Code != "call_locked" {
		t.Errorf("Sam reads a locked call: %d %s", code, body["data"])
	}
	if code, body := callRead(t, callsController.ActInCall, http.MethodPost, "/calls/"+got.Session.ID+"/act", people["sam"], `{"action":"unlock"}`, got.Session.ID); code != http.StatusForbidden {
		t.Errorf("Sam, not in the call, unlocked it: %d %s", code, body["msg"])
	}
	if code, _ := callRead(t, callsController.GetCall, http.MethodGet, "/calls/"+got.Session.ID, people["outsider"], "", got.Session.ID); code != http.StatusNotFound {
		t.Errorf("someone outside the channel read the call by its id: %d", code)
	}
}
