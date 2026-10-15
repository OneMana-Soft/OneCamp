//go:build integration
// +build integration

package integration_test

// Calls against a real call server (livekit/livekit-server:v1.11.0, the version
// the customer stack pins), with real Redis and Dgraph where the rule lives
// there. Each test is one of the calls programme's batch 1 bugs
// (design/calls-vs-zoom/P0.md), and fails on the code before its fix.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestCall' -v

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	lka "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/tests/integration"
)

// join connects a participant with a token, recording what it receives.
type joined struct {
	room         *lksdk.Room
	mu           sync.Mutex
	subscribed   []livekit.TrackSource
	packets      []lksdk.UserDataPacket
	packetSender []string
	disconnected atomic.Value // lksdk.DisconnectionReason
}

func join(t *testing.T, env *integration.LiveKitEnv, token string) *joined {
	t.Helper()
	j := &joined{}
	cb := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(_ *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
				j.mu.Lock()
				j.subscribed = append(j.subscribed, pub.Source())
				j.mu.Unlock()
			},
			OnDataPacket: func(data lksdk.DataPacket, params lksdk.DataReceiveParams) {
				if p, ok := data.(*lksdk.UserDataPacket); ok {
					j.mu.Lock()
					j.packets = append(j.packets, *p)
					j.packetSender = append(j.packetSender, params.SenderIdentity)
					j.mu.Unlock()
				}
			},
		},
		OnDisconnectedWithReason: func(reason lksdk.DisconnectionReason) {
			j.disconnected.Store(reason)
		},
	}
	room, err := lksdk.ConnectToRoomWithToken(env.WSURL, token, cb)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	j.room = room
	t.Cleanup(room.Disconnect)
	return j
}

// publishTrack puts a silent track on the call as the given source.
func publishTrack(room *lksdk.Room, source livekit.TrackSource) error {
	codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	if source == livekit.TrackSource_CAMERA {
		codec = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
	}
	track, err := lksdk.NewLocalTrack(codec)
	if err != nil {
		return err
	}
	track.OnBind(func() {
		_ = track.StartWrite(lksdk.NewNullSampleProvider(64000), func() {})
	})
	_, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name: source.String(), Source: source})
	return err
}

// settled waits until server data reaches j: a participant removed in its
// first moments, before its data channel opens, is put out without the reason
// (its client still learns it was removed, from the disconnect itself).
func joinSettled(t *testing.T, room, identity string, j *joined) {
	t.Helper()
	eventually(t, identity+"'s data channel to open", func() bool {
		_ = livekitInit.LiveKitService.SendData(context.Background(), room, "test.settle", []byte("{}"), identity)
		time.Sleep(200 * time.Millisecond)
		j.mu.Lock()
		defer j.mu.Unlock()
		return slices.ContainsFunc(j.packets, func(p lksdk.UserDataPacket) bool { return p.Topic == "test.settle" })
	})
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// CALL-BUG-1. Join with your microphone and camera off, then turn them on:
// the call server accepts both, and the others receive them. The token used to
// leave "microphone" and "camera" out when they were off in the green room,
// and the server refused every later Unmute, for members, guests and an
// instant meeting's host alike. The last part shows the server enforces that
// (a token of the old shape is refused), so the token is what matters.
func TestCallAMutedJoinerCanUnmuteAndTurnTheCameraOn(t *testing.T) {
	env := integration.SetupLiveKit(t)
	ctx := context.Background()
	room := "grant-" + uuid.NewString()
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}

	listenerToken, err := livekitInit.LiveKitService.GenerateToken(room, "0xa0", "Listener", false, "")
	if err != nil {
		t.Fatal(err)
	}
	listener := join(t, env, listenerToken)

	member, err := livekitInit.LiveKitService.GenerateToken(room, "0xa1", "Maya", false, "en-US")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := livekitInit.LiveKitService.GenerateGuestToken(room, "guest-"+uuid.NewString()+"-x", "Guest: Ann", "")
	if err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{"member": member, "guest": guest} {
		p := join(t, env, token)
		perms := p.room.LocalParticipant.Permissions()
		for _, s := range []livekit.TrackSource{livekit.TrackSource_MICROPHONE, livekit.TrackSource_CAMERA, livekit.TrackSource_SCREEN_SHARE} {
			if perms == nil || (len(perms.CanPublishSources) > 0 && !slices.Contains(perms.CanPublishSources, s)) {
				t.Fatalf("%s: the call server says they may not publish %s: %+v", name, s, perms)
			}
		}
		if err := publishTrack(p.room, livekit.TrackSource_MICROPHONE); err != nil {
			t.Fatalf("%s joined muted and could not unmute: %v", name, err)
		}
		if err := publishTrack(p.room, livekit.TrackSource_CAMERA); err != nil {
			t.Fatalf("%s joined with the camera off and could not turn it on: %v", name, err)
		}
	}
	eventually(t, "the listener to receive the unmuted microphones and cameras", func() bool {
		listener.mu.Lock()
		defer listener.mu.Unlock()
		mics, cams := 0, 0
		for _, s := range listener.subscribed {
			switch s {
			case livekit.TrackSource_MICROPHONE:
				mics++
			case livekit.TrackSource_CAMERA:
				cams++
			}
		}
		return mics == 2 && cams == 2
	})

	// The old shape: screen share only, as minted for someone who joined with
	// both off.
	at := lka.NewAccessToken(env.Key, env.Secret)
	grant := &lka.VideoGrant{RoomJoin: true, Room: room, CanPublishSources: []string{"screen_share", "screen_share_audio"}}
	grant.SetCanPublish(true)
	grant.SetCanSubscribe(true)
	old, err := at.SetVideoGrant(grant).SetIdentity("0xa9").SetValidFor(time.Minute).ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	if err := publishTrack(join(t, env, old).room, livekit.TrackSource_MICROPHONE); err == nil {
		t.Fatal("the call server let a token without microphone publish one; the fix would not be what matters")
	}
}

// CALL-BUG-8. The room is made when the first token is minted. It closed 5
// seconds later if nobody had connected (there at 4 s, gone at 6 s on this
// server), so a slow first join ended the call before it began.
func TestCallAnEmptyRoomWaitsForASlowFirstJoin(t *testing.T) {
	integration.SetupLiveKit(t)
	ctx := context.Background()
	room := "slow-" + uuid.NewString()
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	time.Sleep(8 * time.Second)
	if _, err := livekitInit.LiveKitService.GetRoom(ctx, room); err != nil {
		t.Fatalf("the room closed before its first person arrived: %v", err)
	}
}

// CALL-BUG-11. A host puts someone out of a call: they are told why, by the
// server, and disconnected; they can't mint a token to walk back in while the
// call goes on; and once it ends they can join again.
func TestCallAHostCanPutSomeoneOut(t *testing.T) {
	env := integration.SetupLiveKit(t)
	integration.SetupRedis(t)
	ctx := context.Background()
	room := uuid.NewString()
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	token, err := livekitInit.LiveKitService.GenerateToken(room, "0xb2", "Sam", false, "")
	if err != nil {
		t.Fatal(err)
	}
	sam := join(t, env, token)
	joinSettled(t, room, "0xb2", sam)

	if err := liveKitBusiness.RemoveFromCall(ctx, room, "0xb2", "Maya Chen", liveKitBusiness.RemovedByHost); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Sam to be disconnected", func() bool { return sam.disconnected.Load() != nil })
	if reason := sam.disconnected.Load().(lksdk.DisconnectionReason); reason != lksdk.ParticipantRemoved {
		t.Errorf("disconnected because %q, want removed", reason)
	}
	sam.mu.Lock()
	var told map[string]any
	for i, p := range sam.packets {
		if p.Topic == "oc.call" {
			_ = json.Unmarshal(p.Payload, &told)
			if sam.packetSender[i] != "" {
				t.Errorf("the removal notice came from participant %q, not the server", sam.packetSender[i])
			}
		}
	}
	sam.mu.Unlock()
	if told["t"] != "removed" || told["by"] != "Maya Chen" || told["reason"] != "host" {
		t.Errorf("Sam was told %v", told)
	}
	if people, _ := livekitInit.LiveKitService.ConnectedParticipants(ctx, room); len(people) != 0 {
		t.Errorf("still in the call: %v", people)
	}

	user := &dgraphStruct.DgraphUser{Uid: "0xb2", UserName: "sam"}
	if _, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, user, false, ""); !errors.Is(err, liveKitBusiness.ErrRemovedFromCall) {
		t.Fatalf("a removed member minted a token back into the call: %v", err)
	}
	liveKitBusiness.ForgetCall(ctx, room) // the call ended
	if _, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, user, false, ""); err != nil {
		t.Fatalf("after the call, they can't join its next one: %v", err)
	}
}

// toldRemoved answers the reason in the removal notice j received, "" if none.
func toldRemoved(j *joined) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, p := range j.packets {
		var told map[string]any
		if p.Topic == "oc.call" && json.Unmarshal(p.Payload, &told) == nil && told["t"] == "removed" {
			r, _ := told["reason"].(string)
			return r
		}
	}
	return ""
}

func stillIn(ctx context.Context, room string) []string {
	people, _ := livekitInit.LiveKitService.ConnectedParticipants(ctx, room)
	var ids []string
	for _, p := range people {
		ids = append(ids, p.Identity)
	}
	slices.Sort(ids)
	return ids
}

// CALL-BUG-11. Everyone whose right to a call goes is put out of it, told why:
// a guest whose link an admin revokes (and nobody from another link), every
// guest when guest access is turned off (and not the host), and a member
// removed from the channel. Each used to stay in, with a token that kept
// connecting.
func TestCallPeopleArePutOutWhenTheirRightGoes(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	lk := integration.SetupLiveKit(t)

	hostUUID, samUUID := uuid.New(), uuid.New()
	channelUUID := uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:h", "dgraph.type": "User", "user_uuid": hostUUID.String(), "user_name": "maya"},
		{"uid": "_:s", "dgraph.type": "User", "user_uuid": samUUID.String(), "user_name": "sam"},
		{"uid": "_:c", "dgraph.type": "Channel", "ch_uuid": channelUUID, "ch_name": "launch",
			"ch_members": []map[string]any{{"uid": "_:h"}, {"uid": "_:s"}}},
	})
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'maya@example.test', 'maya', 'maya', NOW(), NOW())`, hostUUID); err != nil {
		t.Fatal(err)
	}
	host := &dgraphStruct.DgraphUser{Uid: uids["h"], Uuid: hostUUID.String(), UserName: "maya"}

	meeting, err := guestBusiness.CreateInstantMeeting(ctx, host, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := guestBusiness.NewMeetingLink(ctx, meeting.Room, host, false)
	if err != nil {
		t.Fatal(err)
	}
	enter := func(room, identity string, guest bool) *joined {
		var token string
		var err error
		if guest {
			token, err = livekitInit.LiveKitService.GenerateGuestToken(room, identity, "Guest", "")
		} else {
			token, err = livekitInit.LiveKitService.GenerateToken(room, identity, "Member", false, "")
		}
		if err != nil {
			t.Fatal(err)
		}
		j := join(t, lk, token)
		joinSettled(t, room, identity, j)
		return j
	}
	first := "guest-" + meeting.GrantID.String() + "-a1"
	other := "guest-" + second.GrantID.String() + "-b2"
	a := enter(meeting.Room, first, true)
	b := enter(meeting.Room, other, true)
	enter(meeting.Room, host.Uid, false)

	if err := guestBusiness.RevokeGrant(ctx, meeting.GrantID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the revoked link's guest to be put out", func() bool { return a.disconnected.Load() != nil })
	if r := toldRemoved(a); r != "link_revoked" {
		t.Errorf("the revoked link's guest was told %q", r)
	}
	if got := stillIn(ctx, meeting.Room); !slices.Contains(got, other) || !slices.Contains(got, host.Uid) {
		t.Errorf("revoking one link put out more than its guests: left %v", got)
	}

	guestBusiness.RemoveAllMeetingGuests(ctx) // guest access turned off
	eventually(t, "every guest to be put out", func() bool { return b.disconnected.Load() != nil })
	if got := stillIn(ctx, meeting.Room); len(got) != 1 || got[0] != host.Uid {
		t.Errorf("with guest access off, left in the meeting: %v, want only its host", got)
	}

	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, channelUUID); err != nil {
		t.Fatal(err)
	}
	sam := enter(channelUUID, uids["s"], false)
	if err := channelBusiness.DeleteChannelMemberEdge(ctx, uids["c"], uids["s"], samUUID.String(), channelUUID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the removed member to be put out of the channel's call", func() bool { return sam.disconnected.Load() != nil })
	if r := toldRemoved(sam); r != "not_member" {
		t.Errorf("the removed member was told %q", r)
	}
}

// CALL-BUG-11. Who may put people out of a call: a channel's moderators and
// the workspace's admins who are in it, an instant meeting's host; nobody in
// a DM or a group, and no other member.
func TestCallWhoMayRemoveSomeone(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.SetupLiveKit(t)

	hostUUID, memberUUID := uuid.New(), uuid.New()
	channelUUID := uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:h", "dgraph.type": "User", "user_uuid": hostUUID.String(), "user_name": "maya"},
		{"uid": "_:m", "dgraph.type": "User", "user_uuid": memberUUID.String(), "user_name": "sam"},
		{"uid": "_:c", "dgraph.type": "Channel", "ch_uuid": channelUUID, "ch_name": "launch",
			"ch_members": []map[string]any{{"uid": "_:h"}, {"uid": "_:m"}}, "ch_moderators": []map[string]any{{"uid": "_:h"}}},
	})
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'maya@example.test', 'maya', 'maya', NOW(), NOW())`, hostUUID); err != nil {
		t.Fatal(err)
	}
	moderator := &dgraphStruct.DgraphUser{Uid: uids["h"], Uuid: hostUUID.String()}
	member := &dgraphStruct.DgraphUser{Uid: uids["m"], Uuid: memberUUID.String()}
	meeting, err := guestBusiness.CreateInstantMeeting(ctx, moderator, "")
	if err != nil {
		t.Fatal(err)
	}
	dm := strings.ReplaceAll(uuid.NewString(), "-", "")
	group := hostUUID.String() + " " + memberUUID.String()

	for _, c := range []struct {
		name  string
		room  string
		who   *dgraphStruct.DgraphUser
		admin bool
		want  bool
	}{
		{"a channel's moderator", channelUUID, moderator, false, true},
		{"a channel's plain member", channelUUID, member, false, false},
		{"a workspace admin in the channel", channelUUID, member, true, true},
		{"anyone in a DM", dm, moderator, true, false},
		{"anyone in a group", group, moderator, true, false},
		{"an instant meeting's host", meeting.Room, moderator, false, true},
		{"another member in an instant meeting", meeting.Room, member, true, false},
		{"nobody", channelUUID, nil, true, false},
	} {
		got, err := liveKitBusiness.MayRemoveFromCall(ctx, c.room, c.who, c.admin)
		if err != nil || got != c.want {
			t.Errorf("%s: may remove = %v (%v), want %v", c.name, got, err, c.want)
		}
	}
}

// CALL-BUG-2. Transcription is the call's switch, written by the server for
// everyone in it, and a recording stopping in the same call leaves it alone
// (StopRecording used to write the whole metadata as {"isRecording": false}).
func TestCallTranscriptionIsTheCallsSwitch(t *testing.T) {
	env := integration.SetupLiveKit(t)
	integration.SetupRedis(t)
	t.Setenv("TRANSCRIPTION_MODE", "frontend")
	ctx := context.Background()
	room := uuid.NewString()
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	token, _ := livekitInit.LiveKitService.GenerateToken(room, "0xc3", "Maya", false, "")
	join(t, env, token)

	sw, err := liveKitBusiness.SetTranscription(ctx, room, "0xc3", "Maya", true)
	if err != nil {
		t.Fatal(err)
	}
	if !sw.On || sw.By != "Maya" || sw.Where != "browser" || !sw.Kept {
		t.Fatalf("switch = %+v", sw)
	}
	read := func() (liveKitBusiness.CallView, string) {
		view, err := liveKitBusiness.ReadCallView(ctx, room, true)
		if err != nil {
			t.Fatal(err)
		}
		r, err := livekitInit.LiveKitService.GetRoom(ctx, room)
		if err != nil {
			t.Fatal(err)
		}
		return view, r.Sid
	}
	view, sid := read()
	if !view.Transcription.On || view.Transcription.By != "Maya" || !view.Active || len(view.Participants) != 1 {
		t.Fatalf("the green room sees %+v", view)
	}

	// A recording starting and stopping in the call, as StartRecording and
	// StopRecording write the metadata.
	for _, keys := range []map[string]any{
		{"isRecording": true, "recordingStartedBy": "Sam", "egressID": "EG_1"},
		{"isRecording": false, "recordingStartedBy": nil, "egressID": nil},
	} {
		if _, err := livekitInit.LiveKitService.UpdateRoomMetadata(ctx, room, keys); err != nil {
			t.Fatal(err)
		}
	}
	if view, _ := read(); !view.Transcription.On || view.Recording.On {
		t.Fatalf("after a recording stopped: %+v", view)
	}
	if got, _ := liveKitBusiness.CallSwitches(ctx, room, sid); !got.On {
		t.Fatal("the server's copy of the switch is off while the call says on")
	}

	if _, err := liveKitBusiness.SetTranscription(ctx, room, "0xc3", "Maya", false); err != nil {
		t.Fatal(err)
	}
	if view, _ := read(); view.Transcription.On {
		t.Fatalf("switched off, the call still says on: %+v", view.Transcription)
	}
	if got, _ := liveKitBusiness.CallSwitches(ctx, room, sid); got.On {
		t.Fatal("switched off, the server still keeps lines")
	}
	// Another session in the same room starts with nothing switched on.
	if got, _ := liveKitBusiness.CallSwitches(ctx, room, "RM_another"); got.On {
		t.Fatal("a later call inherited the switch")
	}
}

// CALL-BUG-2 and CALL-BUG-3. A browser's line is kept only while the call's
// transcription is on, and it goes to the call's own transcript: linked to its
// channel, listed with the channel's recordings once the call ends, deletable
// there, and reached by retention. The repair at boot ends old transcripts and
// only counts ones nobody can reach: it deletes nothing (calls O3).
func TestCallATranscriptIsKeptOnlyWhileOnAndIsListedDeletableAndRetained(t *testing.T) {
	env := integration.SetupLiveKit(t)
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	t.Setenv("TRANSCRIPTION_MODE", "frontend")
	ctx := context.Background()

	channelUUID := uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:u", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "maya"},
		{"uid": "_:c", "dgraph.type": "Channel", "ch_uuid": channelUUID, "ch_name": "launch", "ch_members": []map[string]any{{"uid": "_:u"}}},
	})
	maya := uids["u"]

	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, channelUUID); err != nil {
		t.Fatal(err)
	}
	token, _ := livekitInit.LiveKitService.GenerateToken(channelUUID, maya, "Maya", false, "")
	join(t, env, token)
	room, err := livekitInit.LiveKitService.GetRoom(ctx, channelUUID)
	if err != nil {
		t.Fatal(err)
	}
	key := helpers.CallSessionKeyFor(room.Sid)
	line := liveKitBusiness.CallLine{RoomName: channelUUID, Identity: maya, Text: "I approve the budget", Timestamp: time.Now().UnixMilli()}

	if err := liveKitBusiness.StoreCallLine(ctx, line); !errors.Is(err, liveKitBusiness.ErrTranscriptionOff) {
		t.Fatalf("a line was kept with the call's transcription off: %v", err)
	}
	if _, err := liveKitBusiness.SetTranscription(ctx, channelUUID, maya, "Maya", true); err != nil {
		t.Fatal(err)
	}
	if err := liveKitBusiness.StoreCallLine(ctx, line); err != nil {
		t.Fatalf("a line was refused with the call's transcription on: %v", err)
	}
	if uid, linked, err := recordingDomain.FindCallSessionNode(ctx, key); err != nil || uid == "" || !linked {
		t.Fatalf("the call's transcript is not linked to its channel: %q %v %v", uid, linked, err)
	}

	// A recording that has started but whose node isn't written yet: the line
	// goes to the call's own transcript, and no node is made for the egress
	// (a line used to make one, linked to nothing, kept for ever).
	liveKitBusiness.NoteRecording(ctx, channelUUID, "EG_not_written_yet")
	line.Text, line.Timestamp = "and the timeline", time.Now().UnixMilli()
	if err := liveKitBusiness.StoreCallLine(ctx, line); err != nil {
		t.Fatalf("a line during a recording still being set up was refused: %v", err)
	}
	if uid, _, _ := recordingDomain.FindCallSessionNode(ctx, "EG_not_written_yet"); uid != "" {
		t.Fatal("a transcript line made a node of its own for a recording")
	}
	liveKitBusiness.ForgetRecording(ctx, channelUUID, "EG_not_written_yet")
	var count struct {
		Q []struct {
			N int `json:"n"`
		} `json:"q"`
	}
	raw := dg.Query(t, `{ q(func: eq(recording_egress_id, "`+key+`")) { n: count(recording_transcript) } }`)
	if err := json.Unmarshal(raw, &count); err != nil || len(count.Q) != 1 || count.Q[0].N != 2 {
		t.Fatalf("the call's transcript holds %s, want both lines", raw)
	}

	listed := func() *dgraphStruct.DgraphRecording {
		ch, err := channelDomain.GetChannelAllRecordingList(ctx, channelUUID, maya, "", "", 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range ch.Recordings {
			if r.EgressId == key {
				return r
			}
		}
		return nil
	}
	if listed() != nil {
		t.Fatal("a live call's transcript is listed as if it were over")
	}

	liveKitBusiness.EndCallSession(ctx, key, time.Unix(room.CreationTime, 0))
	rec := listed()
	if rec == nil || !rec.TranscriptOnly {
		t.Fatalf("the ended call's transcript is not listed with the channel's recordings: %+v", rec)
	}
	if r, err := recordingDomain.GetDgraphChannelRecordingInfoByEgressId(ctx, key, maya); err != nil || r == nil {
		t.Fatalf("the channel's delete can't find the transcript: %v", err)
	}
	// A recording that isn't there is told apart from a read that failed, so
	// an outage doesn't answer "Recording not found".
	if _, err := recordingDomain.GetChannelRecordingForPlay(ctx, "call-RM_none", maya); !errors.Is(err, recordingDomain.ErrNotFound) {
		t.Errorf("a missing recording reads as %v, want ErrNotFound", err)
	}
	if _, err := recordingDomain.GetDgraphChannelRecordingInfoByEgressId(ctx, "call-RM_none", maya); !errors.Is(err, recordingDomain.ErrNotFound) {
		t.Errorf("a missing recording to delete reads as %v, want ErrNotFound", err)
	}
	ids, err := recordingDomain.GetRecordingEgressIdsOlderThan(ctx, time.Now().Add(time.Hour))
	if err != nil || !slices.Contains(ids, key) {
		t.Fatalf("retention does not reach the transcript (%v): %v", err, ids)
	}

	// From before calls had an end: a transcript linked to nothing (an instant
	// meeting's, or one whose link failed), its call long over.
	old := time.Now().Add(-72 * time.Hour).UnixMilli()
	dg.Mutate(t, map[string]any{
		"uid": "_:o", "dgraph.type": "Recording", "recording_egress_id": "call-RM_orphan",
		"recording_transcript": []map[string]any{{"uid": "_:l", "dgraph.type": "Recording", "transcript_text": "nobody can read this", "transcript_timestamp": old}},
	})
	rep, err := liveKitBusiness.RepairCallTranscripts(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unreachable != 1 {
		t.Errorf("repair: %s", rep)
	}
	// Nothing is deleted until the owner decides (calls O3): it may be a real
	// conversation's transcript whose link failed once. The node and its line
	// are both still there, and running the repair again changes nothing.
	if uid, _, _ := recordingDomain.FindCallSessionNode(ctx, "call-RM_orphan"); uid == "" {
		t.Error("the repair deleted a transcript")
	}
	if again, err := liveKitBusiness.RepairCallTranscripts(ctx, time.Now()); err != nil || again.Ended != 0 || again.Unreachable != 1 {
		t.Errorf("a second repair: %s %v", again, err)
	}
	count.Q = nil
	raw = dg.Query(t, `{ q(func: eq(recording_egress_id, "call-RM_orphan")) { n: count(recording_transcript) } }`)
	if err := json.Unmarshal(raw, &count); err != nil || len(count.Q) != 1 || count.Q[0].N != 1 {
		t.Errorf("the unreachable transcript holds %s after the repair, want its line", raw)
	}
	if listed() == nil {
		t.Error("the repair touched a live conversation's transcript")
	}
}

// CALL-BUG-17 and CALL-BUG-18. An instant meeting's host, and nobody else,
// can come back to it (its token and link lived only in the page that started
// it), make it a new link, and ask the in-call assistant there (whose access
// check parsed meet-<uuid> as a channel and refused everyone). A meeting whose
// links are all gone is over.
func TestCallOnlyAnInstantMeetingsHostCanComeBackToIt(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.SetupLiveKit(t)

	hostUUID, otherUUID := uuid.New(), uuid.New()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:h", "dgraph.type": "User", "user_uuid": hostUUID.String(), "user_name": "maya"},
		{"uid": "_:o", "dgraph.type": "User", "user_uuid": otherUUID.String(), "user_name": "sam"},
	})
	for id, name := range map[uuid.UUID]string{hostUUID: "maya", otherUUID: "sam"} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	host := &dgraphStruct.DgraphUser{Uid: uids["h"], Uuid: hostUUID.String(), UserName: "maya"}
	other := &dgraphStruct.DgraphUser{Uid: uids["o"], Uuid: otherUUID.String(), UserName: "sam"}

	meeting, err := guestBusiness.CreateInstantMeeting(ctx, host, "en-GB")
	if err != nil {
		t.Fatal(err)
	}
	room, ok := guestBusiness.MeetingRoom(meeting.MeetingID)
	if !ok || room != meeting.Room {
		t.Fatalf("meeting id %q gives room %q, want %q", meeting.MeetingID, room, meeting.Room)
	}

	if token, err := guestBusiness.RejoinMeeting(ctx, room, host, ""); err != nil || token == "" {
		t.Fatalf("the host could not come back to their meeting: %v", err)
	}
	if _, err := guestBusiness.RejoinMeeting(ctx, room, other, ""); !errors.Is(err, guestBusiness.ErrNotHost) {
		t.Fatalf("someone else got the host's token: %v", err)
	}
	if _, err := guestBusiness.NewMeetingLink(ctx, room, other, false); !errors.Is(err, guestBusiness.ErrNotHost) {
		t.Fatalf("someone else made a link: %v", err)
	}

	// The assistant's access rule for a meeting: its host, nobody else.
	if ok, err := liveKitBusiness.MemberMayBeInCall(ctx, room, host.Uid); err != nil || !ok {
		t.Fatalf("the host may not be in their own meeting: %v", err)
	}
	if ok, _ := liveKitBusiness.MemberMayBeInCall(ctx, room, other.Uid); ok {
		t.Fatal("a member who isn't the host may be in a meeting as a member")
	}

	// A new link, turning the old one off: the meeting goes on.
	link, err := guestBusiness.NewMeetingLink(ctx, room, host, true)
	if err != nil {
		t.Fatal(err)
	}
	if g, err := guestModel.GetByID(ctx, meeting.GrantID); err != nil || g == nil || g.RevokedAt == nil {
		t.Errorf("the old link was not turned off: %+v %v", g, err)
	}
	if g, err := guestModel.GetByID(ctx, link.GrantID); err != nil || g == nil || g.RevokedAt != nil {
		t.Fatalf("the new link is not live: %+v %v", g, err)
	}

	// Every link gone: the meeting is over.
	if _, err := guestModel.RevokeLiveForResource(ctx, guestModel.ResourceMeeting, room); err != nil {
		t.Fatal(err)
	}
	if _, err := guestBusiness.RejoinMeeting(ctx, room, host, ""); !errors.Is(err, guestBusiness.ErrNotFound) {
		t.Fatalf("a meeting with no live link: %v, want not found", err)
	}
}
