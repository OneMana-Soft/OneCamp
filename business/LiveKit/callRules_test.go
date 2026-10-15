package business

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"testing"
	"time"

	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"google.golang.org/protobuf/proto"
)

// A call keeps a transcript only where it has a conversation to keep it in.
// An instant meeting has none, so its lines went to a node linked to nothing
// (CALL-BUG-3); it is transcribed for captions only.
func TestOnlyACallWithAConversationKeepsATranscript(t *testing.T) {
	for room, want := range map[string]bool{
		"4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33":                                      true, // a channel
		"4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33 5a1d3b2f-4c8e-4d66-8b62-1e9c2a7b3d44": true, // a DM
		"0123456789abcdef0123456789abcdef":                                          true, // a group
		"meet-6f1c0e2a-1111-4222-8333-944455556666":                                 false,
		"": false,
	} {
		if got := keepsTranscript(room); got != want {
			t.Errorf("keepsTranscript(%q) = %v, want %v", room, got, want)
		}
	}
}

// What a removed person's client is told, from the server, before LiveKit
// disconnects them (the CALLS-FE contract's "oc.call" packet).
func TestRemovedPacketSaysWhoAndWhy(t *testing.T) {
	var got map[string]any
	if err := json.Unmarshal(removedPacket("Maya Chen", RemovedByHost), &got); err != nil {
		t.Fatal(err)
	}
	if got["v"] != float64(1) || got["t"] != "removed" || got["by"] != "Maya Chen" || got["reason"] != "host" {
		t.Errorf("packet = %v", got)
	}
	var revoked map[string]any
	if err := json.Unmarshal(removedPacket("", RemovedLinkRevoked), &revoked); err != nil {
		t.Fatal(err)
	}
	if _, named := revoked["by"]; named || revoked["reason"] != "link_revoked" {
		t.Errorf("a revoked link names nobody: %v", revoked)
	}
}

// The green room reads recording and transcription from the room's metadata,
// whatever else is in it.
func TestParseRoomMeta(t *testing.T) {
	m := parseRoomMeta(`{"isRecording":true,"recordingStartedBy":"Sam","egressID":"EG_1","transcription":{"on":true,"by":"Maya","byId":"0x1","since":5,"where":"browser","kept":true},"other":1}`)
	if !m.IsRecording || m.RecordingStartedBy != "Sam" || m.Transcription == nil || !m.Transcription.On || m.Transcription.Where != "browser" {
		t.Errorf("parsed %+v", m)
	}
	if got := parseRoomMeta("not json"); got.IsRecording || got.Transcription != nil {
		t.Errorf("unreadable metadata switched something on: %+v", got)
	}
}

// CALL-BUG-3. The boot repair ends a linked transcript whose call is long over
// (so it is listed, deletable and under retention), reports one linked to
// nothing once its call is long over (and never deletes it), and leaves
// everything else alone: a call that may still be live, a real recording, one
// already ended or deleted.
func TestDecideSessionRepair(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	old, recent := now.Add(-48*time.Hour), now.Add(-time.Hour)
	for _, c := range []struct {
		name string
		node recordingDomain.CallSessionNode
		want sessionRepair
	}{
		{"linked, long over", recordingDomain.CallSessionNode{Key: "call-RM_a", Linked: true, StartedAt: old, LastLine: old}, repairEnd},
		{"linked, maybe live", recordingDomain.CallSessionNode{Key: "call-RM_b", Linked: true, StartedAt: recent, LastLine: recent}, repairNothing},
		{"linked, ended", recordingDomain.CallSessionNode{Key: "call-RM_c", Linked: true, StartedAt: old, EndedAt: old}, repairNothing},
		{"linked, deleted", recordingDomain.CallSessionNode{Key: "call-RM_d", Linked: true, StartedAt: old, DeletedAt: old}, repairNothing},
		{"orphan, long over", recordingDomain.CallSessionNode{Key: "call-RM_e", LastLine: old}, repairUnreachable},
		{"orphan, no times", recordingDomain.CallSessionNode{Key: "call-RM_f"}, repairUnreachable},
		{"orphan, recent", recordingDomain.CallSessionNode{Key: "call-RM_g", LastLine: recent}, repairNothing},
		{"a recording", recordingDomain.CallSessionNode{Key: "EG_x", StartedAt: old}, repairNothing},
	} {
		if got := decideSessionRepair(c.node, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// The repair pages through every node, ends what it should, deletes nothing
// (calls O3 is the owner's), and running it again ends nothing more.
func TestRepairCallTranscriptsIsIdempotent(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	nodes := map[string]*recordingDomain.CallSessionNode{}
	var order []string
	add := func(n recordingDomain.CallSessionNode) {
		nodes[n.Uid] = &n
		order = append(order, n.Uid)
	}
	add(recordingDomain.CallSessionNode{Uid: "0x1", Key: "call-RM_a", Linked: true, StartedAt: old, LastLine: old})
	add(recordingDomain.CallSessionNode{Uid: "0x2", Key: "call-RM_b", LastLine: old})
	add(recordingDomain.CallSessionNode{Uid: "0x3", Key: "call-RM_c", Linked: true, StartedAt: now})

	keepNodes, keepMark := callSessionNodes, markCallSessionEnded
	t.Cleanup(func() {
		callSessionNodes, markCallSessionEnded = keepNodes, keepMark
	})
	callSessionNodes = func(_ context.Context, after string, first int) ([]recordingDomain.CallSessionNode, error) {
		var page []recordingDomain.CallSessionNode
		for _, uid := range order {
			if n, ok := nodes[uid]; ok && uid > after && len(page) < first {
				page = append(page, *n)
			}
		}
		return page, nil
	}
	markCallSessionEnded = func(_ context.Context, key string, at time.Time) error {
		for _, n := range nodes {
			if n.Key == key {
				n.EndedAt = at
			}
		}
		return nil
	}

	rep, err := RepairCallTranscripts(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ended != 1 || rep.Unreachable != 1 {
		t.Errorf("first run: %s", rep)
	}
	if nodes["0x1"].EndedAt != old {
		t.Errorf("a linked transcript ends when its last line was spoken, got %v", nodes["0x1"].EndedAt)
	}
	if n := nodes["0x2"]; !n.EndedAt.IsZero() {
		t.Error("a transcript linked to nothing was changed")
	}
	if rep, err := RepairCallTranscripts(context.Background(), now); err != nil || rep.Ended != 0 {
		t.Errorf("a second run changed things: %s %v", rep, err)
	}
}

// fakeLiveKit answers the room service calls these rules make: one room with
// a session, and the participants given.
func fakeLiveKit(t *testing.T, room, sid string, participants ...string) {
	t.Helper()
	const key, secret = "test-key", "test-secret-long-enough-to-sign-with"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/protobuf")
		var out proto.Message
		switch path.Base(r.URL.Path) {
		case "ListRooms":
			var req livekit.ListRoomsRequest
			_ = proto.Unmarshal(body, &req)
			resp := &livekit.ListRoomsResponse{}
			for _, name := range req.Names {
				if name == room {
					resp.Rooms = append(resp.Rooms, &livekit.Room{Name: room, Sid: sid})
				}
			}
			out = resp
		case "ListParticipants":
			resp := &livekit.ListParticipantsResponse{}
			for _, id := range participants {
				resp.Participants = append(resp.Participants, &livekit.ParticipantInfo{Identity: id, State: livekit.ParticipantInfo_ACTIVE})
			}
			out = resp
		default:
			out = &livekit.Room{}
		}
		b, _ := proto.Marshal(out)
		_, _ = w.Write(b)
	}))
	t.Cleanup(server.Close)
	saved := livekitInit.LiveKitService
	t.Cleanup(func() { livekitInit.LiveKitService = saved })
	livekitInit.LiveKitService = livekitInit.LiveKitServiceStruct{
		LiveKitClient: lksdk.NewRoomServiceClient(server.URL, key, secret),
		EgressClient:  lksdk.NewEgressClient(server.URL, key, secret),
		Config:        &livekitInit.LiveKitConfigStruct{HostURL: server.URL, ApiKey: key, ApiSecret: secret},
	}
}

// CALL-BUG-2. A line is kept only while somebody in the call has turned its
// transcription on (which told everyone in it). Before, every final sentence
// any Chrome or Edge member's browser recognised was kept, whether or not
// anyone had asked, and nobody was told. Here the caller is in the call and
// nobody turned it on: the line is refused.
func TestALineIsRefusedWhileTheCallsTranscriptionIsOff(t *testing.T) {
	const room = "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33"
	fakeLiveKit(t, room, "RM_one", "0x1")
	t.Setenv("TRANSCRIPTION_MODE", "frontend")

	err := StoreCallLine(context.Background(), CallLine{RoomName: room, Identity: "0x1", Text: "I approve the budget"})
	if !errors.Is(err, ErrTranscriptionOff) {
		t.Fatalf("got %v, want ErrTranscriptionOff", err)
	}
}

// One transcriber per mode: the browser's lines in browser mode, the server's
// in server mode, or every line is kept twice (CALL-BUG-5). And none at all
// while the workspace's transcription is off.
func TestALineFromTheWrongTranscriberIsRefused(t *testing.T) {
	const room = "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33"
	fakeLiveKit(t, room, "RM_one", "0x1")
	for _, c := range []struct {
		mode       string
		fromServer bool
		want       error
	}{
		{"frontend", true, ErrNotBrowserMode},
		{"backend", false, ErrNotBrowserMode},
		{"off", false, ErrTranscriptionDisabled},
		{"off", true, ErrTranscriptionDisabled},
	} {
		t.Setenv("TRANSCRIPTION_MODE", c.mode)
		err := StoreCallLine(context.Background(), CallLine{RoomName: room, Identity: "0x1", Text: "words", FromServer: c.fromServer})
		if !errors.Is(err, c.want) {
			t.Errorf("mode %s, from server %v: got %v, want %v", c.mode, c.fromServer, err, c.want)
		}
	}
}

// Somebody not connected to the call can't have words kept in it, whatever
// they post.
func TestALineFromSomeoneNotInTheCallIsRefused(t *testing.T) {
	const room = "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33"
	fakeLiveKit(t, room, "RM_one", "0x1")
	t.Setenv("TRANSCRIPTION_MODE", "frontend")
	err := StoreCallLine(context.Background(), CallLine{RoomName: room, Identity: "0x2", Text: "words"})
	if !errors.Is(err, ErrNotInCall) {
		t.Fatalf("got %v, want ErrNotInCall", err)
	}
}

// Turning transcription on is refused while the workspace's mode is off, and
// for someone not in the call.
func TestTheSwitchNeedsTheWorkspaceAndThePerson(t *testing.T) {
	const room = "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33"
	fakeLiveKit(t, room, "RM_one", "0x1")

	t.Setenv("TRANSCRIPTION_MODE", "off")
	if _, err := SetTranscription(context.Background(), room, "0x1", "Maya", true); !errors.Is(err, ErrTranscriptionDisabled) {
		t.Errorf("mode off: got %v", err)
	}
	t.Setenv("TRANSCRIPTION_MODE", "frontend")
	if _, err := SetTranscription(context.Background(), room, "0x9", "Sam", true); !errors.Is(err, ErrNotInCall) {
		t.Errorf("not in the call: got %v", err)
	}
	if _, err := SetTranscription(context.Background(), "no-such-room", "0x1", "Maya", true); !errors.Is(err, ErrNotInCall) {
		t.Errorf("no call: got %v", err)
	}
}
