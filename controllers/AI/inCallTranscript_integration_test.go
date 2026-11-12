//go:build integration

package controllers

// The in-call assistant answers from the call going on, and from nothing
// else (UX-CALLS-H3, CALLS-05).
//
// When the asker's browser has heard nothing (a late joiner, captions just
// turned on), the server reads the transcript kept so far. It read "the
// newest recording in this room": with no line kept yet in the call going
// on, that was the channel's PREVIOUS call, so someone asking "what did we
// decide?" was told what an earlier call decided, people who weren't in it
// included. And once someone recorded part of the call, it read the
// recording alone, dropping everything said before it started.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	callState "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/akashc777/OneCamp/tests/livekitfake"
	"github.com/google/uuid"
)

// inCallChannel is a channel whose previous call kept a transcript and a
// recording, ended hours ago: Maya and Jonas's words in it must never reach
// a later call's assistant.
type inCallChannel struct {
	channel    string
	channelUID string
	maya       string
	jonas      string
	dg         *integration.DgraphEnv
}

const earlierCallWords = "We set the budget at five thousand."

func seedInCallChannel(t *testing.T) inCallChannel {
	t.Helper()
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	channel := uuid.NewString()
	began, ended := time.Now().Add(-3*time.Hour), time.Now().Add(-150*time.Minute)
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "Maya Chen"},
		{"uid": "_:jonas", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "Jonas Berg"},
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel, "ch_name": "launch",
			"ch_deleted_at": "0001-01-01T00:00:00Z",
			"ch_members":    []map[string]any{{"uid": "_:maya"}, {"uid": "_:jonas"}},
			"ch_recording":  []map[string]any{{"uid": "_:earlier"}, {"uid": "_:earlierRec"}}},
		// The earlier call's own transcript, and a recording made during it.
		{"uid": "_:earlier", "dgraph.type": "Recording", "recording_egress_id": inCallKey("call-RM_"),
			"recording_transcript_only": true, "recording_stared_at": began, "recording_ended_at": ended,
			"recording_channel": map[string]any{"uid": "_:ch"},
			"recording_transcript": []map[string]any{
				{"dgraph.type": "Transcript", "transcript_text": earlierCallWords, "transcript_timestamp": began.Add(time.Minute).UnixMilli(), "transcript_from": map[string]any{"uid": "_:maya"}},
			}},
		{"uid": "_:earlierRec", "dgraph.type": "Recording", "recording_egress_id": inCallKey("EG_"),
			"recording_stared_at": began.Add(5 * time.Minute), "recording_ended_at": ended,
			"recording_obj_key": "recordings/launch/earlier.mp4", "recording_size": 5000,
			"recording_channel": map[string]any{"uid": "_:ch"},
			"recording_transcript": []map[string]any{
				{"dgraph.type": "Transcript", "transcript_text": "Jonas owns the vendor call.", "transcript_timestamp": began.Add(6 * time.Minute).UnixMilli(), "transcript_from": map[string]any{"uid": "_:jonas"}},
			}},
	})
	return inCallChannel{channel: channel, channelUID: uids["ch"], maya: uids["maya"], jonas: uids["jonas"], dg: dg}
}

// inCallKey is a key as LiveKit makes one: a prefix, then twelve letters and digits.
func inCallKey(prefix string) string {
	return prefix + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
}

// startCall opens the channel's call going on now, as the first join does,
// begun at began in LiveKit's room sid.
func (c inCallChannel) startCall(t *testing.T, sid string, began time.Time) {
	t.Helper()
	if _, made, err := callState.OpenSession(context.Background(), callState.Session{
		ID: uuid.New(), Room: c.channel, RoomSID: sid, Kind: callState.KindHuddle,
		ResourceKind: callState.ResourceChannel, ResourceID: c.channel, StartedAt: began,
	}); err != nil || !made {
		t.Fatalf("the call going on: made %v, %v", made, err)
	}
}

// heardBy is what the assistant reads for someone connected to the call
// going on, in LiveKit's room sid: the call ConnectedCall answers them.
func (c inCallChannel) heardBy(t *testing.T, sid, uid string) string {
	t.Helper()
	livekitfake.Serve(t, c.channel, sid, uid)
	call, err := liveKitBusiness.ConnectedCall(context.Background(), c.channel, uid)
	if err != nil {
		t.Fatalf("someone in the call has no call: %v", err)
	}
	return fetchPersistedTranscript(context.Background(), call)
}

// TestInCallFallbackReadsOnlyTheCallGoingOn: a channel whose previous call
// kept a transcript gives the assistant nothing from it, with no call going
// on in it, and with a call going on that has kept no line yet. The model is
// then told nothing has been heard, and says so.
func TestInCallFallbackReadsOnlyTheCallGoingOn(t *testing.T) {
	c := seedInCallChannel(t)
	ctx := context.Background()

	livekitfake.Serve(t, "another-room", inCallKey("RM_"), c.maya)
	if _, err := liveKitBusiness.ConnectedCall(ctx, c.channel, c.maya); err == nil {
		t.Errorf("no call going on in the channel: the assistant had a call to read")
	}

	sid := inCallKey("RM_")
	c.startCall(t, sid, time.Now().Add(-10*time.Minute))
	if got := c.heardBy(t, sid, c.maya); got != "" {
		t.Errorf("a call going on that has kept no line: the assistant read %q, want nothing (the channel's earlier call is not this one)", got)
	}
}

// TestInCallFallbackReadsALateJoinersCall: someone who joins late reads what
// the call going on has kept, in the order it was said: the lines on the
// call's own transcript, and the lines on a recording made during it, and
// never the earlier call's.
func TestInCallFallbackReadsALateJoinersCall(t *testing.T) {
	c := seedInCallChannel(t)

	sid := inCallKey("RM_")
	began := time.Now().Add(-20 * time.Minute)
	c.startCall(t, sid, began)
	recordedFrom := began.Add(10 * time.Minute)
	uids := c.dg.Mutate(t, []map[string]any{
		{"uid": "_:session", "dgraph.type": "Recording", "recording_egress_id": "call-" + sid,
			"recording_transcript_only": true, "recording_stared_at": began,
			"recording_channel": map[string]any{"uid": c.channelUID},
			"recording_transcript": []map[string]any{
				{"dgraph.type": "Transcript", "transcript_text": "Let's ship v2 on Friday.", "transcript_timestamp": began.Add(2 * time.Minute).UnixMilli(), "transcript_from": map[string]any{"uid": c.jonas}},
				{"dgraph.type": "Transcript", "transcript_text": "Agreed, Friday it is.", "transcript_timestamp": began.Add(3 * time.Minute).UnixMilli(), "transcript_from": map[string]any{"uid": c.maya}},
			}},
		// Someone started recording part-way: the lines since go to it.
		{"uid": "_:recording", "dgraph.type": "Recording", "recording_egress_id": inCallKey("EG_"),
			"recording_stared_at": recordedFrom,
			"recording_channel":   map[string]any{"uid": c.channelUID},
			"recording_transcript": []map[string]any{
				{"dgraph.type": "Transcript", "transcript_text": "I'll write the release notes.", "transcript_timestamp": recordedFrom.Add(time.Minute).UnixMilli(), "transcript_from": map[string]any{"uid": c.maya}},
			}},
	})
	c.dg.Mutate(t, map[string]any{"uid": c.channelUID, "ch_recording": []map[string]any{{"uid": uids["session"]}, {"uid": uids["recording"]}}})

	want := "Jonas Berg: Let's ship v2 on Friday.\nMaya Chen: Agreed, Friday it is.\nMaya Chen: I'll write the release notes."
	got := c.heardBy(t, sid, c.maya)
	if got != want {
		t.Errorf("a late joiner's assistant read\n%s\nwant this call's lines, in the order they were said:\n%s", got, want)
	}
	if strings.Contains(got, earlierCallWords) || strings.Contains(got, "vendor call") {
		t.Errorf("a late joiner's assistant read the channel's earlier call: %q", got)
	}
}

// TestInCallFallbackReadsTheNewestOfALongCall: in a call that has kept more
// lines than the assistant reads, it reads the newest, the talk a late
// joiner is catching up on. It read the first ones, so an hour into a call
// its answers were about the first few minutes.
func TestInCallFallbackReadsTheNewestOfALongCall(t *testing.T) {
	c := seedInCallChannel(t)

	sid := inCallKey("RM_")
	began := time.Now().Add(-90 * time.Minute)
	c.startCall(t, sid, began)
	const said = inCallFallbackLines + 50
	lines := make([]map[string]any, 0, said)
	for i := 1; i <= said; i++ {
		lines = append(lines, map[string]any{"dgraph.type": "Transcript", "transcript_text": "Point " + strconv.Itoa(i) + ".",
			"transcript_timestamp": began.Add(time.Duration(i) * 10 * time.Second).UnixMilli(), "transcript_from": map[string]any{"uid": c.maya}})
	}
	uids := c.dg.Mutate(t, []map[string]any{
		{"uid": "_:session", "dgraph.type": "Recording", "recording_egress_id": "call-" + sid,
			"recording_transcript_only": true, "recording_stared_at": began,
			"recording_channel": map[string]any{"uid": c.channelUID}, "recording_transcript": lines},
	})
	c.dg.Mutate(t, map[string]any{"uid": c.channelUID, "ch_recording": []map[string]any{{"uid": uids["session"]}}})

	got := strings.Split(c.heardBy(t, sid, c.maya), "\n")
	first, last := "Maya Chen: Point "+strconv.Itoa(said-inCallFallbackLines+1)+".", "Maya Chen: Point "+strconv.Itoa(said)+"."
	if len(got) != inCallFallbackLines || got[0] != first || got[len(got)-1] != last {
		t.Errorf("a call that kept %d lines: the assistant read %d, from %q to %q; want the newest %d, from %q to %q",
			said, len(got), got[0], got[len(got)-1], inCallFallbackLines, first, last)
	}
}

// askInCall asks the call's assistant as the person whose graph uid is given,
// and answers the status and the words.
func (c inCallChannel) askInCall(t *testing.T, uid, heard string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(inCallAskRequest{RoomID: c.channel, Question: "What did we decide?", RecentTranscript: heard})
	who := userModels.UserInfo{}
	who.UserDgraphInfo.Uid = uid
	who.UserDgraphInfo.Uuid = uuid.NewString()
	req := httptest.NewRequest(http.MethodPost, "/ai/in-call/ask/stream", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	InCallAskStream(rec, req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, who)))
	var out struct {
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Msg
}

// TestOnlySomeoneInTheCallMayAskItsAssistant (V-860): a live call's transcript
// is for the people connected to it. A member of the channel who isn't in the
// call was answered from what it had kept, silently, while the people in it
// saw a list of who was there. They are refused, in the words anyone outside
// the channel reads, whether or not they send lines of their own; someone in
// the call goes on to the model (switched off in this test: 503).
func TestOnlySomeoneInTheCallMayAskItsAssistant(t *testing.T) {
	c := seedInCallChannel(t)
	sid := inCallKey("RM_")
	c.startCall(t, sid, time.Now().Add(-10*time.Minute))
	livekitfake.Serve(t, c.channel, sid, c.maya)

	if code, msg := c.askInCall(t, c.maya, ""); code != http.StatusServiceUnavailable || msg != "AI is not enabled" {
		t.Errorf("someone in the call: %d %q, want to reach the model (503 \"AI is not enabled\" here)", code, msg)
	}
	for name, heard := range map[string]string{"nothing heard": "", "lines of their own": "Maya Chen: hello"} {
		if code, msg := c.askInCall(t, c.jonas, heard); code != http.StatusForbidden || msg != "You're not in this call." {
			t.Errorf("a channel member who isn't in the call, %s: %d %q, want 403 \"You're not in this call.\"", name, code, msg)
		}
	}

	// The call over, nobody is in it: the same refusal, for everyone.
	livekitfake.Serve(t, "another-room", inCallKey("RM_"))
	if code, msg := c.askInCall(t, c.jonas, ""); code != http.StatusForbidden || msg != "You're not in this call." {
		t.Errorf("no call going on: %d %q, want 403 \"You're not in this call.\"", code, msg)
	}
}

// TestTheAskLeavesWithThem (V-860 F1): the ask takes presence from the call
// now, for this call and this person, never from the 60 s grant the write
// path caches per room. Someone who left, someone a host put out, and someone
// from the room's previous call are refused at once, their grant still warm.
func TestTheAskLeavesWithThem(t *testing.T) {
	c := seedInCallChannel(t)
	integration.SetupRedis(t)
	ctx := context.Background()
	inCall := func(when, uid string) {
		t.Helper()
		if code, msg := c.askInCall(t, uid, ""); code != http.StatusServiceUnavailable || msg != "AI is not enabled" {
			t.Errorf("%s: %d %q, want to reach the model (503 \"AI is not enabled\" here)", when, code, msg)
		}
	}
	refused := func(when, uid string) {
		t.Helper()
		if code, msg := c.askInCall(t, uid, "Maya Chen: hello"); code != http.StatusForbidden || msg != "You're not in this call." {
			t.Errorf("%s: %d %q, want 403 \"You're not in this call.\"", when, code, msg)
		}
	}
	// warm files a line of their own, as browser transcription does, so the
	// grant is cached; with no cache this test proves nothing.
	warm := func(uid string) {
		t.Helper()
		_ = redisStore.Delete(ctx, registry.CallTranscriptGrant, []string{c.channel, uid})
		if _, err := liveKitBusiness.ResolveTranscriptSession(ctx, c.channel, uid); err != nil {
			t.Fatalf("%s files a line: %v", uid, err)
		}
		if key, ok, err := redisStore.GetString(ctx, registry.CallTranscriptGrant, []string{c.channel, uid}); err != nil || !ok || key == "" {
			t.Fatalf("no grant cached for %s (%q %v %v): the test needs a warm cache", uid, key, ok, err)
		}
	}

	sid := inCallKey("RM_")
	c.startCall(t, sid, time.Now().Add(-10*time.Minute))
	lk := livekitfake.Serve(t, c.channel, sid, c.maya, c.jonas)
	warm(c.maya)
	warm(c.jonas)
	inCall("Maya, connected", c.maya)
	inCall("Jonas, connected", c.jonas)

	// (a) Maya closes the tab.
	lk.Leave(c.channel, c.maya)
	refused("Maya, the moment after she left", c.maya)

	// (b) A host puts Jonas out.
	if err := liveKitBusiness.RemoveFromCall(ctx, c.channel, c.jonas,
		liveKitBusiness.Remover{Identity: c.maya, Name: "Maya Chen"}, liveKitBusiness.RemovedByHost); err != nil {
		t.Fatalf("the host's removal: %v", err)
	}
	refused("Jonas, the moment after a host put him out", c.jonas)

	// (c) Maya comes back; the call ends and the room's next call begins at
	// once, under a new sid, with Jonas in it and not Maya, whose grant is
	// from the last call.
	lk.Room(c.channel, sid, c.maya)
	warm(c.maya)
	open, err := callState.OpenSessionOf(ctx, c.channel)
	if err != nil || open == nil {
		t.Fatalf("the call going on: %v %v", open, err)
	}
	if ended, err := callState.EndSession(ctx, open.ID, callState.EndedReconciled, time.Now()); err != nil || !ended {
		t.Fatalf("ending the call: %v %v", ended, err)
	}
	next := inCallKey("RM_")
	c.startCall(t, next, time.Now())
	lk.Room(c.channel, next, c.jonas)
	refused("Maya, in the room's last call and not its next", c.maya)
	inCall("Jonas, in the room's next call", c.jonas)
}
