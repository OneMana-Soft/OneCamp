//go:build integration

package business

// A LIVE call's transcript is for the people connected to that call; a
// FINISHED call's is for its conversation's members, as the recap already
// shares it with them (V-860).
//
// The agent tool read_meeting_transcript and the task extractor read "the
// room's newest recording" for any member of its conversation. While a call
// was running and keeping lines, that was the call itself: a member who had
// not joined read what was being said, silently, while the people in it saw
// a list of who was there.

import (
	"context"
	"strings"
	"testing"
	"time"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	callState "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/akashc777/OneCamp/tests/livekitfake"
	"github.com/google/uuid"
)

const (
	finishedCallWords = "We set the budget at five thousand."
	liveCallWords     = "The launch moves to Friday."
	nextCallWords     = "Lena is out until Monday."
)

// transcriptReader is one of the AI's readers of a room's newest transcript,
// answering what it read for a person ("" for a refusal or nothing).
type transcriptReader func(ctx context.Context, t *testing.T, who uuid.UUID, uid, channel string) string

var transcriptReaders = map[string]transcriptReader{
	"read_meeting_transcript": func(ctx context.Context, t *testing.T, who uuid.UUID, _, channel string) string {
		out, _, err := executeReadMeetingTranscript(ctx, ai.ProposedAction{Params: map[string]string{"channel_uuid": channel}}, who.String())
		if err != nil {
			if err.Error() != "no call found for that channel, or you do not have access to it" {
				t.Errorf("read_meeting_transcript refused in other words: %v", err)
			}
			return ""
		}
		return out
	},
	"the task extractor": func(ctx context.Context, _ *testing.T, who uuid.UUID, uid, channel string) string {
		info := &userModels.UserInfo{}
		info.UserDgraphInfo.Uid = uid
		info.UserDgraphInfo.Uuid = who.String()
		info.UserDgraphInfo.Channels = []*dgraphStruct.DgraphChannel{{Uuid: channel}}
		out, _ := gatherMeetingTranscript(ctx, info, channel)
		return out
	},
}

// liveChannel is a channel whose members are Maya, Jonas and Lena, with a
// call that finished hours ago and kept a line.
type liveChannel struct {
	channel, channelUID string
	who                 map[string]uuid.UUID // by first name, lower case
	uid                 map[string]string    // graph uid, by the same names
	dg                  *integration.DgraphEnv
}

func seedLiveChannel(t *testing.T) liveChannel {
	t.Helper()
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)

	c := liveChannel{channel: uuid.NewString(), who: map[string]uuid.UUID{}, uid: map[string]string{}, dg: dg}
	earlier := time.Now().Add(-3 * time.Hour)
	zero := "0001-01-01T00:00:00Z"
	nodes := []map[string]any{
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": c.channel, "ch_name": "launch", "ch_deleted_at": zero,
			"ch_members":   []map[string]any{{"uid": "_:maya"}, {"uid": "_:jonas"}, {"uid": "_:lena"}},
			"ch_recording": []map[string]any{{"uid": "_:earlier"}}},
		{"uid": "_:earlier", "dgraph.type": "Recording", "recording_egress_id": "call-" + liveSID(),
			"recording_transcript_only": true, "recording_stared_at": earlier, "recording_ended_at": earlier.Add(30 * time.Minute),
			"recording_channel": map[string]any{"uid": "_:ch"},
			"recording_transcript": []map[string]any{
				{"dgraph.type": "Transcript", "transcript_text": finishedCallWords, "transcript_timestamp": earlier.Add(time.Minute).UnixMilli(), "transcript_from": map[string]any{"uid": "_:maya"}},
			}},
	}
	for _, name := range []string{"maya", "jonas", "lena"} {
		id := uuid.New()
		c.who[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name+"-"+id.String()[:8], name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(),
			"user_name": strings.ToUpper(name[:1]) + name[1:], "user_channels": []map[string]any{{"uid": "_:ch"}}})
	}
	uids := dg.Mutate(t, nodes)
	c.channelUID = uids["ch"]
	for name := range c.who {
		c.uid[name] = uids[name]
	}
	return c
}

// liveSID is a room sid as LiveKit makes one.
func liveSID() string { return "RM_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12] }

// startCall opens a call going on now in room (the channel's, or one of its
// breakout rooms when parent is set), under LiveKit's room sid.
func (c liveChannel) startCall(t *testing.T, room, sid string, parent *uuid.UUID) *callState.Session {
	t.Helper()
	kind := callState.KindHuddle
	if parent != nil {
		kind = callState.KindBreakout
	}
	call, made, err := callState.OpenSession(context.Background(), callState.Session{
		ID: uuid.New(), Room: room, RoomSID: sid, Kind: kind, ParentSessionID: parent,
		ResourceKind: callState.ResourceChannel, ResourceID: c.channel, StartedAt: time.Now().Add(-10 * time.Minute),
	})
	if err != nil || !made {
		t.Fatalf("the call going on in %s: made %v, %v", room, made, err)
	}
	return call
}

// keeps files words under the call (or breakout room) whose sid is given, as
// the channel's newest transcript, the way a call's own lines are kept.
func (c liveChannel) keeps(t *testing.T, sid, words, by string) {
	t.Helper()
	began := time.Now().Add(-10 * time.Minute)
	live := c.dg.Mutate(t, []map[string]any{
		{"uid": "_:session", "dgraph.type": "Recording", "recording_egress_id": "call-" + sid,
			"recording_transcript_only": true, "recording_stared_at": began,
			"recording_channel": map[string]any{"uid": c.channelUID},
			"recording_transcript": []map[string]any{
				{"dgraph.type": "Transcript", "transcript_text": words, "transcript_timestamp": began.Add(time.Minute).UnixMilli(), "transcript_from": map[string]any{"uid": c.uid[by]}},
			}},
	})
	c.dg.Mutate(t, map[string]any{"uid": c.channelUID, "ch_recording": []map[string]any{{"uid": live["session"]}}})
}

// reads checks what name reads through every reader of the room's newest
// transcript: want in it, or a refusal when want is "".
func (c liveChannel) reads(t *testing.T, when, name, want string) {
	t.Helper()
	for reader, read := range transcriptReaders {
		got := read(context.Background(), t, c.who[name], c.uid[name], c.channel)
		if want == "" && got != "" {
			t.Errorf("%s: %s read through %s, want a refusal:\n%s", when, name, reader, got)
		}
		if want != "" && !strings.Contains(got, want) {
			t.Errorf("%s: %s read %q through %s, want %q in it", when, name, got, reader, want)
		}
	}
}

func TestALiveCallsTranscriptIsForThePeopleInIt(t *testing.T) {
	c := seedLiveChannel(t)

	// No call going on: the finished call is the channel's members' to read.
	c.reads(t, "no call going on", "jonas", finishedCallWords)

	// A call begins; Maya is in it, Jonas is not. Until it keeps a line, the
	// channel's newest transcript is still the finished call's.
	sid := liveSID()
	call := c.startCall(t, c.channel, sid, nil)
	livekitfake.Serve(t, c.channel, sid, c.uid["maya"])
	c.reads(t, "a call going on that has kept nothing, a member who isn't in it", "jonas", finishedCallWords)

	// The call keeps a line: the newest transcript is now the live call's.
	c.keeps(t, sid, liveCallWords, "maya")
	c.reads(t, "a live call, a member who isn't in it", "jonas", "")
	c.reads(t, "a live call, someone in it", "maya", liveCallWords)

	// The call ends: its transcript is the channel's members' to read.
	if ended, err := callState.EndSession(context.Background(), call.ID, callState.EndedReconciled, time.Now()); err != nil || !ended {
		t.Fatalf("ending the call: %v %v", ended, err)
	}
	c.reads(t, "the call over, a member who wasn't in it", "jonas", liveCallWords)
}

// warm files a line of identity's own in room, as browser transcription
// does, so the server has cached that they are in its call (the grant), and
// checks it did: with no cache this test proves nothing.
func warm(t *testing.T, room, identity string) {
	t.Helper()
	ctx := context.Background()
	_ = redisStore.Delete(ctx, registry.CallTranscriptGrant, []string{room, identity})
	if _, err := liveKitBusiness.ResolveTranscriptSession(ctx, room, identity); err != nil {
		t.Fatalf("%s files a line in %s: %v", identity, room, err)
	}
	if key, ok, err := redisStore.GetString(ctx, registry.CallTranscriptGrant, []string{room, identity}); err != nil || !ok || key == "" {
		t.Fatalf("no grant cached for %s in %s (%q %v %v): the test needs a warm cache", identity, room, key, ok, err)
	}
}

// TestALiveCallsTranscriptLeavesWithThem (V-860 F1): presence is the call's
// now, for this call and this person. The readers took it from the 60 s grant
// the write path caches per room, so someone who left, someone a host put
// out, and someone from the room's previous call read the call going on for
// up to a minute. Each is refused at once, with their grant still warm.
func TestALiveCallsTranscriptLeavesWithThem(t *testing.T) {
	c := seedLiveChannel(t)
	integration.SetupRedis(t)
	ctx := context.Background()

	sid := liveSID()
	call := c.startCall(t, c.channel, sid, nil)
	lk := livekitfake.Serve(t, c.channel, sid, c.uid["maya"], c.uid["jonas"], c.uid["lena"])
	c.keeps(t, sid, liveCallWords, "maya")
	for _, name := range []string{"maya", "jonas", "lena"} {
		warm(t, c.channel, c.uid[name])
		c.reads(t, "connected", name, liveCallWords)
	}

	// (a) Maya closes the tab.
	lk.Leave(c.channel, c.uid["maya"])
	c.reads(t, "the moment after she left", "maya", "")

	// (b) A host puts Jonas out.
	if err := liveKitBusiness.RemoveFromCall(ctx, c.channel, c.uid["jonas"],
		liveKitBusiness.Remover{Identity: c.uid["lena"], Name: "Lena"}, liveKitBusiness.RemovedByHost); err != nil {
		t.Fatalf("the host's removal: %v", err)
	}
	c.reads(t, "the moment after a host put him out", "jonas", "")

	// (c) The call ends; the room's next call begins at once, under a new
	// sid, with Jonas in it and not Lena, whose grant is from the last call.
	if ended, err := callState.EndSession(ctx, call.ID, callState.EndedReconciled, time.Now()); err != nil || !ended {
		t.Fatalf("ending the call: %v %v", ended, err)
	}
	next := liveSID()
	c.startCall(t, c.channel, next, nil)
	lk.Room(c.channel, next, c.uid["jonas"])
	c.keeps(t, next, nextCallWords, "jonas")
	c.reads(t, "the room's next call, someone from the last one", "lena", "")
	c.reads(t, "the room's next call, someone in it", "jonas", nextCallWords)
}

// TestABreakoutRoomsTranscriptIsForThePeopleInIt (V-860 T1): a call's
// breakout rooms keep their lines where the call does, so presence counts in
// any of them, and a line kept in one makes the newest transcript the live
// call's. Each half is pinned: main room silent, a breakout room has kept a
// line, someone connected to neither is refused; someone connected only to
// the breakout room reads it.
func TestABreakoutRoomsTranscriptIsForThePeopleInIt(t *testing.T) {
	c := seedLiveChannel(t)

	sid, roomSID := liveSID(), liveSID()
	call := c.startCall(t, c.channel, sid, nil)
	breakout := callStateBusiness.BreakoutRoom(c.channel, 1)
	c.startCall(t, breakout, roomSID, &call.ID)
	lk := livekitfake.Serve(t, c.channel, sid, c.uid["maya"])
	lk.Room(breakout, roomSID, c.uid["lena"])
	c.keeps(t, roomSID, liveCallWords, "lena")

	c.reads(t, "the main room silent, a breakout room keeping lines, a member in neither", "jonas", "")
	c.reads(t, "connected only to the breakout room", "lena", liveCallWords)
}
