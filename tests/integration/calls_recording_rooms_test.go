//go:build integration

package integration_test

// A call's recording goes on after its breakout rooms (meet-10, zoom-02; main's
// condition on both rows, 28 Oct). A recording is LiveKit's egress in the
// call's own room, and LiveKit closes that room 20 s after its last person
// leaves it for a breakout room: a recorder isn't a person, so it doesn't hold
// the room, and the recording ended with it. Nothing recorded the call again
// when everyone came back, so its record had one recording, cut at the rooms.
//
// A real call server makes and closes the rooms. The recorder is a fake egress
// service behind LiveKit's own egress API (integration.FakeEgress). The
// recording is started and stopped through the channel's own routes, as its
// host's screen does, and LiveKit's webhooks reach the server's own handler,
// signed as LiveKit signs them.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/encoding/protojson"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	livekitController "github.com/akashc777/OneCamp/controllers/LiveKit"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/pgexec"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// lkWebhook hands the server's own webhook handler a LiveKit event, signed as
// LiveKit signs one: a token from its key, carrying the body's sha256.
func lkWebhook(t *testing.T, lk *integration.LiveKitEnv, event *livekit.WebhookEvent) {
	t.Helper()
	event.CreatedAt = time.Now().Unix()
	body, err := protojson.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	token, err := auth.NewAccessToken(lk.Key, lk.Secret).SetValidFor(time.Minute).SetSha256(base64.StdEncoding.EncodeToString(sum[:])).ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/livekit/webhook", bytes.NewReader(body))
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/webhook+json")
	rec := httptest.NewRecorder()
	livekitController.HandleWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LiveKit's %s: %d %s", event.Event, rec.Code, rec.Body.String())
	}
}

// recordingRoute asks a channel's recording route to start or stop, as its
// call's Record button does, as who.
func recordingRoute(t *testing.T, start bool, room string, who *dgraphStruct.DgraphUser) int {
	t.Helper()
	h, path := channelController.StopVideoChannelCallRecording, "/ch/stopCallRecording"
	if start {
		h, path = channelController.StartVideoChannelCallRecording, "/ch/startCallRecording"
	}
	return call(t, h, http.MethodPost, path, &userModels.UserInfo{UserDgraphInfo: *who}, `{"channel_uuid":"`+room+`"}`, nil).Code
}

// roomRecording is what a room's metadata, which every screen in it reads,
// says of its recording.
func roomRecording(t *testing.T, ctx context.Context, room string) (on bool, by, egress string) {
	t.Helper()
	r, err := livekitInit.LiveKitService.GetRoom(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		On     bool   `json:"isRecording"`
		By     string `json:"recordingStartedBy"`
		Egress string `json:"egressID"`
	}
	if r.Metadata != "" {
		if err := json.Unmarshal([]byte(r.Metadata), &meta); err != nil {
			t.Fatalf("%s's metadata: %v", room, err)
		}
	}
	return meta.On, meta.By, meta.Egress
}

// channelRecordings are the egress ids of the recordings a channel lists.
func channelRecordings(t *testing.T, dg *integration.DgraphEnv, channel string) []string {
	t.Helper()
	var out struct {
		Q []struct {
			Recordings []struct {
				Egress string `json:"recording_egress_id"`
			} `json:"ch_recording"`
		} `json:"q"`
	}
	if err := json.Unmarshal(dg.Query(t, `{ q(func: eq(ch_uuid, "`+channel+`")) { ch_recording { recording_egress_id } } }`), &out); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range out.Q {
		for _, r := range c.Recordings {
			ids = append(ids, r.Egress)
		}
	}
	return ids
}

// recordedCall is a channel's call that its host records, gone into its one
// breakout room, Design, with everyone in it.
type recordedCall struct {
	room   string
	people map[string]*dgraphStruct.DgraphUser
	s      *callStateBusiness.Session
	host   string
	back   string
	// sid is the call's own room as it was recorded; recordedBy what its
	// screens said of who records it.
	sid, recordedBy string
	// design is each person's connection to Design.
	design map[string]*joined
}

// recordIntoRooms starts host's and back's call in a channel of their own
// (outsider isn't in it), has the host record it through the channel's route
// (after a participant is refused), and moves both into Design. The call's own
// room is left empty, as LiveKit finds it before closing it.
func recordIntoRooms(t *testing.T, ctx context.Context, pg *integration.Env, dg *integration.DgraphEnv, lk *integration.LiveKitEnv, egress *integration.FakeEgress, host, back, outsider string) *recordedCall {
	t.Helper()
	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, host, back, outsider)
	s, in := enterCall(t, ctx, lk, room, people, host, back)
	if code := recordingRoute(t, true, room, people[back]); code != http.StatusForbidden {
		t.Fatalf("%s, a participant, starting the recording: %d, want 403", back, code)
	}
	if code := recordingRoute(t, true, room, people[host]); code != http.StatusOK {
		t.Fatalf("%s starting the recording: %d", host, code)
	}
	if started := egress.Started(room); len(started) != 1 {
		t.Fatalf("%d recordings started, want 1", len(started))
	}
	on, by, _ := roomRecording(t, ctx, room)
	if !on || by == "" {
		t.Fatalf("the call's screens don't say it is recorded: on %v, by %q", on, by)
	}
	r, err := livekitInit.LiveKitService.GetRoom(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	act := func(a callControlBusiness.Action) {
		t.Helper()
		if _, err := callControlBusiness.Act(ctx, people[host], s.ID, a); err != nil {
			t.Fatalf("%s doing %s: %v", host, a.Action, err)
		}
	}
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsSet, Rooms: []string{"Design"}})
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsSpread})
	act(callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsOpen})
	design := callStateBusiness.BreakoutRoom(room, 1)
	inDesign := map[string]*joined{}
	for _, name := range []string{back, host} {
		rt, err := callControlBusiness.BreakoutToken(ctx, people[name], s.ID, 1, "")
		if err != nil {
			t.Fatalf("%s's token for Design: %v", name, err)
		}
		j := join(t, lk, rt.Token)
		t.Cleanup(j.room.Disconnect)
		inDesign[name] = j
		joinSettled(t, design, people[name].Uid, j)
		connected(t, ctx, design, people[name].Uid)
		leave(t, ctx, room, people[name].Uid, in[name])
	}
	return &recordedCall{room: room, people: people, s: s, host: host, back: back, sid: r.Sid, recordedBy: by, design: inDesign}
}

// closeOwnRoom closes the call's empty room as LiveKit does after its
// departure grace, and tells the server as LiveKit does (room_finished).
func (c *recordedCall) closeOwnRoom(t *testing.T, ctx context.Context, lk *integration.LiveKitEnv) {
	t.Helper()
	liveKitBusiness.DeleteRoom(ctx, c.room)
	lkWebhook(t, lk, &livekit.WebhookEvent{Event: "room_finished", Room: &livekit.Room{Name: c.room, Sid: c.sid}})
	if now, _ := callStateBusiness.Get(ctx, c.s.ID); now == nil || !now.Live {
		t.Fatalf("closing the call's own room ended the call while everyone was in Design: %+v", now)
	}
}

// comeBack closes the rooms, and brings the participant back first: LiveKit
// makes the call's room again for them, and its webhook says they joined. It
// answers the room's new sid.
func (c *recordedCall) comeBack(t *testing.T, ctx context.Context, lk *integration.LiveKitEnv) string {
	t.Helper()
	if _, err := callControlBusiness.Act(ctx, c.people[c.host], c.s.ID, callControlBusiness.Action{Action: callControlBusiness.ActBreakoutsClose}); err != nil {
		t.Fatalf("closing the rooms: %v", err)
	}
	if first, err := callStateModel.FinishBreakouts(ctx, uuid.MustParse(c.s.ID), time.Now().Add(callStateModel.BreakoutCountdown+time.Second)); err != nil || !first {
		t.Fatalf("calling everyone back: %v, %v", first, err)
	}
	sid := c.rejoin(t, ctx, lk)
	if cur, _ := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: c.room}); cur == nil || cur.ID != c.s.ID {
		t.Fatalf("%s came back to a new call %+v, want the one that went into the rooms", c.back, cur)
	}
	return sid
}

// rejoin brings the participant into the call's room: LiveKit makes it again
// for them, and its webhook says they joined. It answers the room's new sid.
func (c *recordedCall) rejoin(t *testing.T, ctx context.Context, lk *integration.LiveKitEnv) string {
	t.Helper()
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, c.room); err != nil {
		t.Fatal(err)
	}
	token, err := livekitInit.LiveKitService.GenerateToken(c.room, c.people[c.back].Uid, title(c.back), false, "", livekitInit.CallPublishSources)
	if err != nil {
		t.Fatal(err)
	}
	j := join(t, lk, token)
	t.Cleanup(j.room.Disconnect)
	joinSettled(t, c.room, c.people[c.back].Uid, j)
	r, err := livekitInit.LiveKitService.GetRoom(ctx, c.room)
	if err != nil {
		t.Fatal(err)
	}
	if r.Sid == c.sid {
		t.Fatalf("the call's room wasn't made again: still %s", r.Sid)
	}
	var p *livekit.ParticipantInfo
	eventually(t, c.back+" to be back in "+c.room, func() bool {
		p, err = livekitInit.LiveKitService.Participant(ctx, c.room, c.people[c.back].Uid)
		return err == nil && p != nil
	})
	lkWebhook(t, lk, &livekit.WebhookEvent{Event: "participant_joined", Room: &livekit.Room{Name: c.room, Sid: r.Sid}, Participant: p})
	return r.Sid
}

// endRecording ends a recording as LiveKit's egress does, and says so as
// LiveKit does (egress_ended).
func endRecording(t *testing.T, lk *integration.LiveKitEnv, egress *integration.FakeEgress, id string) {
	t.Helper()
	lkWebhook(t, lk, &livekit.WebhookEvent{Event: "egress_ended", EgressInfo: egress.End(t, id)})
}

// settle waits for the work a webhook handed off (the room made again, a
// recording's end) to finish.
func settle(t *testing.T) {
	t.Helper()
	if busy := helpers.SettleBackground(20 * time.Second); len(busy) > 0 {
		t.Fatalf("still running: %v", busy)
	}
}

// sweepCalls runs the reconcile sweep once, as its ticker does, and waits for
// the work it hands off.
func sweepCalls(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := callControlBusiness.Reconcile(ctx); err != nil {
		t.Fatalf("the reconcile sweep: %v", err)
	}
	settle(t)
}

// backdate moves every join to and departure from the call and its rooms d
// earlier, as if d had gone by since: the grace to come back, say.
func (c *recordedCall) backdate(t *testing.T, ctx context.Context, d time.Duration) {
	t.Helper()
	if _, err := pgexec.Exec(ctx, `UPDATE call_attendance SET joined_at = joined_at - $2::interval,
			last_joined_at = last_joined_at - $2::interval, left_at = left_at - $2::interval
		WHERE session_id IN (SELECT id FROM call_sessions WHERE id = $1 OR parent_session_id = $1)`,
		c.s.ID, fmt.Sprintf("%d seconds", int(d.Seconds()))); err != nil {
		t.Fatal(err)
	}
}

func TestCallRecordingGoesOnAfterItsBreakoutRooms(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	egress := lk.UseFakeEgress(t)

	// isResumed checks the call records again in its room made again, as the
	// host who started it, and that its record and its channel list both parts.
	isResumed := func(t *testing.T, c *recordedCall, sid string) {
		t.Helper()
		var started []*livekit.EgressInfo
		eventually(t, "the call to be recorded again", func() bool {
			started = egress.Started(c.room)
			return len(started) == 2
		})
		first, again := started[0], started[1]
		if again.RoomId != sid {
			t.Errorf("the recording went on in room %s, want the room made again, %s", again.RoomId, sid)
		}
		settle(t)
		recs, err := callStateModel.Recordings(ctx, uuid.MustParse(c.s.ID))
		if err != nil || len(recs) != 2 {
			t.Fatalf("the call's record lists %d recordings (%v), want both parts", len(recs), err)
		}
		if recs[0].EgressID != first.EgressId || recs[0].EndedAt == nil || recs[0].Status != "ready" {
			t.Errorf("the first part: %+v, want %s, ended, ready", recs[0], first.EgressId)
		}
		if recs[1].EgressID != again.EgressId || recs[1].EndedAt != nil || recs[1].StartedBy == nil || recs[1].StartedBy.String() != c.people[c.host].Uuid {
			t.Errorf("the second part: %+v, want %s, running, started by %s", recs[1], again.EgressId, c.host)
		}
		if on, by, id := roomRecording(t, ctx, c.room); !on || by != c.recordedBy || id != again.EgressId {
			t.Errorf("the call's screens say recording %v by %q (%s), want on by %q (%s)", on, by, id, c.recordedBy, again.EgressId)
		}
		if ids := channelRecordings(t, dg, c.room); len(ids) != 2 {
			t.Errorf("the channel lists recordings %v, want both parts", ids)
		}
	}

	t.Run("its own room closes while everyone is in Design, and the call records again when they're back", func(t *testing.T) {
		c := recordIntoRooms(t, ctx, pg, dg, lk, egress, "sam", "ana", "outsider")
		c.closeOwnRoom(t, ctx, lk)
		endRecording(t, lk, egress, egress.Started(c.room)[0].EgressId)
		settle(t)
		if started := egress.Started(c.room); len(started) != 1 {
			t.Fatalf("a recording started while everyone was in Design: %d", len(started))
		}
		// Ana, a participant, is back first: the call records as Sam.
		sid := c.comeBack(t, ctx, lk)
		isResumed(t, c, sid)
	})

	t.Run("they're back before the cut recording's end reaches the server, and it records again at that end", func(t *testing.T) {
		c := recordIntoRooms(t, ctx, pg, dg, lk, egress, "maya", "lee", "stranger")
		c.closeOwnRoom(t, ctx, lk)
		sid := c.comeBack(t, ctx, lk)
		settle(t)
		if started := egress.Started(c.room); len(started) != 1 {
			t.Fatalf("a second recording started while the first still ran: %d", len(started))
		}
		// The upload finishes; LiveKit says the recording ended.
		endRecording(t, lk, egress, egress.Started(c.room)[0].EgressId)
		isResumed(t, c, sid)
	})

	t.Run("a recording its host stopped in a breakout room stays stopped", func(t *testing.T) {
		c := recordIntoRooms(t, ctx, pg, dg, lk, egress, "kai", "noa", "visitor")
		if code := recordingRoute(t, false, c.room, c.people["noa"]); code != http.StatusForbidden {
			t.Fatalf("Noa, a participant, stopping the recording: %d, want 403", code)
		}
		if code := recordingRoute(t, false, c.room, c.people["kai"]); code != http.StatusOK {
			t.Fatalf("Kai stopping the recording from Design: %d", code)
		}
		endRecording(t, lk, egress, egress.Started(c.room)[0].EgressId)
		c.closeOwnRoom(t, ctx, lk)
		c.comeBack(t, ctx, lk)
		settle(t)
		if started := egress.Started(c.room); len(started) != 1 {
			t.Errorf("a stopped recording started again: %d recordings", len(started))
		}
		if on, _, _ := roomRecording(t, ctx, c.room); on {
			t.Error("the call's screens say it is recorded after its host stopped it")
		}
		recs, err := callStateModel.Recordings(ctx, uuid.MustParse(c.s.ID))
		if err != nil || len(recs) != 1 || recs[0].EndedAt == nil {
			t.Errorf("the call's record: %+v (%v), want the one stopped recording", recs, err)
		}
	})
}

// Migration call_recording_by goes down and up again, and up twice: a call
// someone keeps recorded stores who.
func TestCallRecordingByMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	column := func() bool {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'call_sessions' AND column_name = 'recording_by'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if !column() {
		t.Fatal("call_sessions has no recording_by after every migration")
	}
	if _, err := env.PG.Exec(migrationSQL(t, "call_recording_by", "down")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if column() {
		t.Fatal("recording_by is still there after down")
	}
	for _, step := range []string{"up again", "up twice"} {
		if _, err := env.PG.Exec(migrationSQL(t, "call_recording_by", "up")); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at, recording_by) VALUES ($1, 'room-r', 'huddle', 'channel', 'room-r', NOW(), $2)`, uuid.New(), uuid.New()); err != nil {
		t.Fatalf("a call someone keeps recorded: %v", err)
	}
}

// A call whose people have all gone while its breakout rooms are open ends
// once nobody has been in its room or any of its rooms for the time the word
// to come back takes (callStateModel.BreakoutReturnGrace), and its rooms end
// with it. Whoever joins later starts a call of their own, which nothing of
// the old one carries on into: its recording doesn't start again.
func TestACallLeftEmptyInItsRoomsEnds(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	egress := lk.UseFakeEgress(t)
	c := recordIntoRooms(t, ctx, pg, dg, lk, egress, "ines", "omar", "passerby")
	c.closeOwnRoom(t, ctx, lk)
	endRecording(t, lk, egress, egress.Started(c.room)[0].EgressId)
	settle(t)
	// Both leave Design too, and close their screens: nobody is anywhere in the call.
	design := callStateBusiness.BreakoutRoom(c.room, 1)
	for _, name := range []string{c.back, c.host} {
		leave(t, ctx, design, c.people[name].Uid, c.design[name])
	}

	// Within the grace to come back, the call goes on.
	sweepCalls(t, ctx)
	if now, _ := callStateBusiness.Get(ctx, c.s.ID); now == nil || !now.Live {
		t.Fatalf("the sweep ended the call within the grace to come back: %+v", now)
	}

	// The grace goes by with nobody back: the call ends, and its rooms with it.
	gone := callStateModel.BreakoutReturnGrace + time.Minute
	c.backdate(t, ctx, gone)
	sweepCalls(t, ctx)
	if now, _ := callStateBusiness.Get(ctx, c.s.ID); now == nil || now.Live {
		t.Errorf("a call nobody has been in for %s, its rooms open, still goes on: %+v", gone, now)
	}
	if open, err := callStateModel.InRooms(ctx, uuid.MustParse(c.s.ID)); err != nil || len(open) != 0 {
		t.Errorf("people still in the ended call's rooms: %v (%v)", open, err)
	}
	var roomsOpen int
	if _, _, err := pgexec.QueryOne(ctx, `SELECT count(*) FROM call_sessions WHERE parent_session_id = $1 AND ended_at IS NULL`,
		func(r pgexec.Row) (int, error) { return roomsOpen, r.Scan(&roomsOpen) }, c.s.ID); err != nil || roomsOpen != 0 {
		t.Errorf("%d of the ended call's rooms still open (%v)", roomsOpen, err)
	}

	// Omar opens the channel's call later: a call of his own, not recorded.
	c.rejoin(t, ctx, lk)
	settle(t)
	if cur, _ := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: c.room}); cur == nil || cur.ID == c.s.ID {
		t.Errorf("joining later went back into the call left in its rooms: %+v", cur)
	}
	if started := egress.Started(c.room); len(started) != 1 {
		t.Errorf("joining later recorded the call again: %d recordings, want the first only", len(started))
	}
	if on, by, _ := roomRecording(t, ctx, c.room); on {
		t.Errorf("the later call's screens say it is recorded, by %q", by)
	}
}

// A call goes on in its rooms for as long as someone is in it, in one of its
// breakout rooms or in its own room while the rooms are open (a host waiting
// there while everyone else works in rooms), however long ago anyone last came
// or went. Only a call nobody is in ends (TestACallLeftEmptyInItsRoomsEnds).
func TestACallWithSomeoneInItGoesOnInItsRooms(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	egress := lk.UseFakeEgress(t)
	// into moves host and back into Design, as recordIntoRooms does, and LiveKit
	// closes the call's own room, left empty, and with it its recording.
	into := func(t *testing.T, host, back, outsider string) *recordedCall {
		t.Helper()
		c := recordIntoRooms(t, ctx, pg, dg, lk, egress, host, back, outsider)
		c.closeOwnRoom(t, ctx, lk)
		endRecording(t, lk, egress, egress.Started(c.room)[0].EgressId)
		settle(t)
		return c
	}
	// goesOn lets the grace to come back go by, twice over, with nobody coming
	// or going, then sweeps: the call must still go on.
	goesOn := func(t *testing.T, c *recordedCall, where string) {
		t.Helper()
		c.backdate(t, ctx, 2*callStateModel.BreakoutReturnGrace)
		sweepCalls(t, ctx)
		if now, _ := callStateBusiness.Get(ctx, c.s.ID); now == nil || !now.Live {
			t.Errorf("the sweep ended the call while %s was in %s: %+v", c.back, where, now)
		}
	}

	t.Run("someone stays in a breakout room", func(t *testing.T) {
		c := into(t, "rosa", "eli", "walker")
		leave(t, ctx, callStateBusiness.BreakoutRoom(c.room, 1), c.people[c.host].Uid, c.design[c.host])
		goesOn(t, c, "Design")
		if in, err := callStateModel.InRooms(ctx, uuid.MustParse(c.s.ID)); err != nil || len(in) != 1 || in[c.people[c.back].Uid] == "" {
			t.Errorf("who is in the call's rooms: %v (%v), want %s alone", in, err, c.back)
		}
	})

	t.Run("someone stays in the call's own room while its rooms are open", func(t *testing.T) {
		c := into(t, "uma", "theo", "bystander")
		for _, name := range []string{c.back, c.host} {
			leave(t, ctx, callStateBusiness.BreakoutRoom(c.room, 1), c.people[name].Uid, c.design[name])
		}
		c.rejoin(t, ctx, lk)
		settle(t)
		goesOn(t, c, "the call's own room")
		if open, err := callStateModel.InRooms(ctx, uuid.MustParse(c.s.ID)); err != nil || len(open) != 0 {
			t.Errorf("people in the call's rooms: %v (%v), want none", open, err)
		}
	})
}
