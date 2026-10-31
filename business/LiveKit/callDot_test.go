package business

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"google.golang.org/protobuf/proto"
)

// Who can be connected to a call's room, as LiveKit lists them.
var (
	aPerson     = &livekit.ParticipantInfo{Identity: "0x1", State: livekit.ParticipantInfo_ACTIVE}
	aDialIn     = &livekit.ParticipantInfo{Identity: "sip_0x2", State: livekit.ParticipantInfo_ACTIVE, Kind: livekit.ParticipantInfo_SIP}
	theAgent    = &livekit.ParticipantInfo{Identity: "transcriber-bot", State: livekit.ParticipantInfo_ACTIVE, Kind: livekit.ParticipantInfo_AGENT}
	aRecorder   = &livekit.ParticipantInfo{Identity: "EG_room", State: livekit.ParticipantInfo_ACTIVE, Kind: livekit.ParticipantInfo_EGRESS, Permission: &livekit.ParticipantPermission{Hidden: true, Recorder: true}}
	aWatcher    = &livekit.ParticipantInfo{Identity: "watcher", State: livekit.ParticipantInfo_ACTIVE, Permission: &livekit.ParticipantPermission{Hidden: true}}
	someoneGone = &livekit.ParticipantInfo{Identity: "0x3", State: livekit.ParticipantInfo_DISCONNECTED}
)

// fakeRooms answers the room service calls behind a call's dot from the rooms
// given, each with who is connected to it: ListRooms (by name, or every room)
// and ListParticipants. A room that isn't given doesn't exist, and LiveKit says
// so as it does. It answers what it was asked. A listing comes back in the
// order the names were asked (every room by name when none were), so a dot
// that stops at the first person asks the same rooms every run.
func fakeRooms(t *testing.T, rooms map[string][]*livekit.ParticipantInfo) *askedOf {
	t.Helper()
	const key, secret = "test-key", "test-secret-long-enough-to-sign-with"
	asked := &askedOf{n: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var out proto.Message
		switch path.Base(r.URL.Path) {
		case "ListRooms":
			var req livekit.ListRoomsRequest
			_ = proto.Unmarshal(body, &req)
			asked.add("ListRooms")
			names := req.Names
			if len(names) == 0 {
				names = slices.Sorted(maps.Keys(rooms))
			}
			resp := &livekit.ListRoomsResponse{}
			for _, name := range names {
				if in, ok := rooms[name]; ok {
					resp.Rooms = append(resp.Rooms, &livekit.Room{Name: name, Sid: "RM_" + name, NumParticipants: uint32(len(in))})
				}
			}
			out = resp
		case "ListParticipants":
			var req livekit.ListParticipantsRequest
			_ = proto.Unmarshal(body, &req)
			asked.add("ListParticipants " + req.Room)
			in, ok := rooms[req.Room]
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"not_found","msg":"requested room does not exist"}`))
				return
			}
			out = &livekit.ListParticipantsResponse{Participants: in}
		default:
			out = &livekit.Room{}
		}
		w.Header().Set("Content-Type", "application/protobuf")
		b, _ := proto.Marshal(out)
		_, _ = w.Write(b)
	}))
	t.Cleanup(server.Close)
	saved := livekitInit.LiveKitService
	t.Cleanup(func() { livekitInit.LiveKitService = saved })
	livekitInit.LiveKitService = livekitInit.LiveKitServiceStruct{
		LiveKitClient: lksdk.NewRoomServiceClient(server.URL, key, secret),
		Config:        &livekitInit.LiveKitConfigStruct{HostURL: server.URL, ApiKey: key, ApiSecret: secret},
	}
	return asked
}

// askedOf counts the requests a fake LiveKit was sent, by method and room. Its
// server writes it while the test reads it.
type askedOf struct {
	mu sync.Mutex
	n  map[string]int
}

func (a *askedOf) add(request string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n[request]++
}

func (a *askedOf) counts() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.n)
}

// roomsOfCalls stands in for Call State's answer to which rooms a call has
// (callRooms): the calls given, each with its rooms, own room first; any other
// room is a call of its own.
func roomsOfCalls(t *testing.T, calls map[string][]string) {
	t.Helper()
	saved := callRooms
	t.Cleanup(func() { callRooms = saved })
	callRooms = func(_ context.Context, room string) ([]string, error) {
		if rooms, ok := calls[room]; ok {
			return rooms, nil
		}
		return []string{room}, nil
	}
}

// V-89. Only a person lights a call's dot, the one a channel, a DM or a group
// shows and the sidebar's: not the transcription agent, a recorder or a hidden
// watcher, nor someone LiveKit still lists after they left. Before, anyone but
// the agent did, so a recorder or a hidden watcher alone lit a call nobody was
// in. It is the rule that ends a call when its last person leaves.
func TestOnlyAPersonLightsACall(t *testing.T) {
	cases := map[string]struct {
		in   []*livekit.ParticipantInfo
		want bool
	}{
		"nobody":                     {nil, false},
		"the transcription agent":    {[]*livekit.ParticipantInfo{theAgent}, false},
		"a recorder":                 {[]*livekit.ParticipantInfo{aRecorder}, false},
		"a hidden watcher":           {[]*livekit.ParticipantInfo{aWatcher}, false},
		"someone who has left":       {[]*livekit.ParticipantInfo{someoneGone}, false},
		"all of those":               {[]*livekit.ParticipantInfo{theAgent, aRecorder, aWatcher, someoneGone}, false},
		"a person":                   {[]*livekit.ParticipantInfo{aPerson}, true},
		"a person who dialled in":    {[]*livekit.ParticipantInfo{aDialIn}, true},
		"a person beside a recorder": {[]*livekit.ParticipantInfo{aRecorder, theAgent, aPerson}, true},
	}
	rooms := map[string][]*livekit.ParticipantInfo{}
	for name, c := range cases {
		rooms[name] = c.in
	}
	fakeRooms(t, rooms)
	roomsOfCalls(t, nil)
	ctx := context.Background()
	lit, err := HumanActiveRoomNames(ctx)
	if err != nil {
		t.Fatalf("the sidebar's dots: %v", err)
	}
	for name, c := range cases {
		if got, err := IsCallActive(ctx, name); err != nil || got != c.want {
			t.Errorf("a room holding %s: its dot lit %v (%v), want %v", name, got, err, c.want)
		}
		if lit[name] != c.want {
			t.Errorf("a room holding %s: its sidebar dot lit %v, want %v", name, lit[name], c.want)
		}
	}
}

// V-89, the breakout corner. While a call's people are all in its breakout
// rooms, its own room holds nobody, or only its recorder, or LiveKit has closed
// it. The call is live all the same: its dot is lit while a person is in any of
// its rooms, and goes out once only recorders, watchers and the agent are left.
func TestACallIsLitWhileAPersonIsInAnyOfItsRooms(t *testing.T) {
	const own = "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33"
	b1, b2 := own+"~b1", own+"~b2"
	roomsOfCalls(t, map[string][]string{own: {own, b1, b2}})
	for name, c := range map[string]struct {
		rooms map[string][]*livekit.ParticipantInfo
		want  bool
	}{
		"its own room holds only its recorder": {map[string][]*livekit.ParticipantInfo{own: {aRecorder}, b1: nil, b2: {aPerson}}, true},
		"its own room is empty":                {map[string][]*livekit.ParticipantInfo{own: nil, b1: {aPerson}}, true},
		"LiveKit closed its own room":          {map[string][]*livekit.ParticipantInfo{b1: {theAgent}, b2: {aPerson}}, true},
		"nobody but recorders and watchers":    {map[string][]*livekit.ParticipantInfo{own: {aRecorder}, b1: {aWatcher}, b2: {theAgent, aRecorder}}, false},
		"every room closed":                    {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			fakeRooms(t, c.rooms)
			if got, err := IsCallActive(context.Background(), own); err != nil || got != c.want {
				t.Errorf("the call's dot lit %v (%v), want %v", got, err, c.want)
			}
		})
	}
}

// V-89, CR-91's S1. The dot of a call whose people are all in its breakout
// rooms (it may have 50) reads them at once: one listing of the breakout
// rooms, then who is in a room only where LiveKit counts someone. The agent
// alone in b7 doesn't light it, so the dot goes on to b42, where a person is.
// Before, it asked who was in each breakout room in turn, one request per
// room, every time the dot was read.
func TestACallsBreakoutRoomsAreReadAtOnce(t *testing.T) {
	const own = "9b2e4d70-1c3a-4f8e-b6d2-7a0c5e9f1b44"
	rooms := map[string][]*livekit.ParticipantInfo{own: nil}
	call := []string{own}
	for i := range 50 {
		breakout := fmt.Sprintf("%s~b%d", own, i+1)
		rooms[breakout] = nil
		call = append(call, breakout)
	}
	agentIn, personIn := own+"~b7", own+"~b42"
	rooms[agentIn] = []*livekit.ParticipantInfo{theAgent}
	rooms[personIn] = []*livekit.ParticipantInfo{aPerson}
	roomsOfCalls(t, map[string][]string{own: call})
	asked := fakeRooms(t, rooms)
	if lit, err := IsCallActive(context.Background(), own); err != nil || !lit {
		t.Fatalf("the call's dot lit %v (%v), want lit", lit, err)
	}
	want := map[string]int{
		"ListParticipants " + own:      1, // its own room first
		"ListRooms":                    1,
		"ListParticipants " + agentIn:  1,
		"ListParticipants " + personIn: 1,
	}
	if got := asked.counts(); !maps.Equal(got, want) {
		t.Errorf("reading the call's dot asked LiveKit %v; want %v", got, want)
	}
}

// CR-93's M1. While a call's people are spread over its breakout rooms, its
// dot stops at the first person it finds: one listing of the breakout rooms,
// then who is in them only until a person turns up. Collecting every room with
// a person in it is the sidebar's job; the dot asked all ten here, to light
// what the first of them already lit.
func TestACallsDotStopsAtTheFirstPersonInItsBreakoutRooms(t *testing.T) {
	const own = "c3a1f0d2-6e4b-4f7a-9d85-2b1e7c4a9f60"
	rooms := map[string][]*livekit.ParticipantInfo{own: nil}
	call := []string{own}
	for i := range 50 {
		breakout := fmt.Sprintf("%s~b%d", own, i+1)
		call = append(call, breakout)
		rooms[breakout] = nil
		if i < 10 {
			rooms[breakout] = []*livekit.ParticipantInfo{aPerson}
		}
	}
	roomsOfCalls(t, map[string][]string{own: call})
	asked := fakeRooms(t, rooms)
	if lit, err := IsCallActive(context.Background(), own); err != nil || !lit {
		t.Fatalf("the call's dot lit %v (%v), want lit", lit, err)
	}
	got, breakouts := asked.counts(), 0
	for request, n := range got {
		if strings.HasPrefix(request, "ListParticipants "+own+"~") {
			breakouts += n
		}
	}
	if got["ListParticipants "+own] != 1 || got["ListRooms"] != 1 || breakouts != 1 {
		t.Errorf("with a person in each of 10 breakout rooms, the dot asked LiveKit %v; want its own room, one listing and one breakout room", got)
	}
}
