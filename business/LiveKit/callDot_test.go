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
// that stops at the first person asks the same rooms every run. The rooms can
// change under it (askedOf.set), and a request can be made to fail as a
// LiveKit that can't answer fails (askedOf.failing).
func fakeRooms(t *testing.T, rooms map[string][]*livekit.ParticipantInfo) *askedOf {
	t.Helper()
	const key, secret = "test-key", "test-secret-long-enough-to-sign-with"
	asked := &askedOf{n: map[string]int{}, rooms: rooms, fails: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var out proto.Message
		rooms := asked.now()
		switch path.Base(r.URL.Path) {
		case "ListRooms":
			var req livekit.ListRoomsRequest
			_ = proto.Unmarshal(body, &req)
			if asked.add("ListRooms") {
				cantAnswer(w)
				return
			}
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
			if asked.add("ListParticipants " + req.Room) {
				cantAnswer(w)
				return
			}
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
	// Another call server has other calls: nothing kept from the last one.
	CallRoomsChanged()
	t.Cleanup(CallRoomsChanged)
	return asked
}

// cantAnswer is LiveKit failing a request: not a missing room, an error.
func cantAnswer(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"code":"internal","msg":"the call server can't answer"}`))
}

// askedOf counts the requests a fake LiveKit was sent, by method and room, and
// holds the rooms it answers from and the requests it fails. Its server reads
// and writes it while the test does.
type askedOf struct {
	mu    sync.Mutex
	n     map[string]int
	rooms map[string][]*livekit.ParticipantInfo
	fails map[string]bool
	// while is what happens while a request is answered (whileAsked).
	while map[string]func()
}

// add counts a request, does what happens while it's answered, and says
// whether it fails.
func (a *askedOf) add(request string) bool {
	a.mu.Lock()
	a.n[request]++
	fails, while := a.fails[request], a.while[request]
	a.mu.Unlock()
	if while != nil {
		while()
	}
	return fails
}

// whileAsked has something happen while a request is being answered (nil:
// nothing), as a webhook comes in while a read is under way.
func (a *askedOf) whileAsked(request string, do func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.while == nil {
		a.while = map[string]func(){}
	}
	a.while[request] = do
}

// now is the rooms as they are, for one answer.
func (a *askedOf) now() map[string][]*livekit.ParticipantInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.rooms)
}

// set changes the rooms, as people come and go.
func (a *askedOf) set(rooms map[string][]*livekit.ParticipantInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rooms = rooms
}

// failing makes a request ("ListRooms", "ListParticipants <room>") fail, or
// answer again.
func (a *askedOf) failing(request string, fails bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fails[request] = fails
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

// H3C (BACKEND-OPT, 19 Nov 2026). The sidebar asked LiveKit for every room on
// every load, then who was in each room someone was in: one listing plus one
// more per call going on, each time anybody opened the app. The answer is kept
// for 3 seconds, so the loads in that time ask LiveKit nothing. What a load is
// handed is its own: changing it changes no other load's.
func TestTheSidebarAsksLiveKitOnceWithinTheTTL(t *testing.T) {
	asked := fakeRooms(t, map[string][]*livekit.ParticipantInfo{"a": {aPerson}, "b": {aPerson}, "quiet": nil})
	roomsOfCalls(t, nil)
	ctx := context.Background()
	for load := 1; load <= 2; load++ {
		lit, err := HumanActiveRoomNames(ctx)
		if err != nil || len(lit) != 2 || !lit["a"] || !lit["b"] {
			t.Fatalf("load %d: the sidebar's dots %v (%v), want a and b lit", load, lit, err)
		}
		lit["someone else's"] = true
	}
	want := map[string]int{"ListRooms": 1, "ListParticipants a": 1, "ListParticipants b": 1}
	if got := asked.counts(); !maps.Equal(got, want) {
		t.Errorf("two sidebar loads within 3 s asked LiveKit %v; want %v, one read for both", got, want)
	}
}

// A read that failed is never kept, nor one that is missing a room: LiveKit's
// listing failing fails that load and the next asks again; a room whose people
// couldn't be read counts as nobody's for that load (the sidebar still
// answers, as it always did), and the next load asks again.
func TestARoomReadThatFailedIsNotKept(t *testing.T) {
	roomsOfCalls(t, nil)
	ctx := context.Background()
	t.Run("LiveKit's listing failed", func(t *testing.T) {
		asked := fakeRooms(t, map[string][]*livekit.ParticipantInfo{"a": {aPerson}})
		asked.failing("ListRooms", true)
		if lit, err := HumanActiveRoomNames(ctx); err == nil {
			t.Fatalf("a listing LiveKit failed lit %v, want its error", lit)
		}
		asked.failing("ListRooms", false)
		if lit, err := HumanActiveRoomNames(ctx); err != nil || !lit["a"] {
			t.Fatalf("the next load's dots %v (%v), want a lit", lit, err)
		}
		if got := asked.counts()["ListRooms"]; got != 2 {
			t.Errorf("a failed listing and the load after it asked LiveKit %d times, want 2: a failure is never kept", got)
		}
	})
	t.Run("one room's people couldn't be read", func(t *testing.T) {
		asked := fakeRooms(t, map[string][]*livekit.ParticipantInfo{"a": {aPerson}, "b": {aPerson}})
		asked.failing("ListParticipants b", true)
		if lit, err := HumanActiveRoomNames(ctx); err != nil || !lit["a"] || lit["b"] {
			t.Fatalf("with b unreadable the sidebar's dots %v (%v), want a lit and b out for this load", lit, err)
		}
		asked.failing("ListParticipants b", false)
		if lit, err := HumanActiveRoomNames(ctx); err != nil || !lit["a"] || !lit["b"] {
			t.Fatalf("the next load's dots %v (%v), want a and b lit: a read missing a room is never kept", lit, err)
		}
		if got := asked.counts(); got["ListRooms"] != 2 || got["ListParticipants b"] != 2 {
			t.Errorf("a read missing b and the load after it asked LiveKit %v, want 2 listings and b read twice", got)
		}
	})
}

// The kept answer only lights the sidebar's dots, rows of the reader's own
// conversations. It never answers whether a call is going on for a join, a
// ring or a recording (IsCallActive), nor whether its room is there to join
// (CheckRoomExists): those ask LiveKit every time, a kept answer or not.
func TestTheJoinPathAsksLiveKitEveryTime(t *testing.T) {
	asked := fakeRooms(t, map[string][]*livekit.ParticipantInfo{"a": {aPerson}})
	roomsOfCalls(t, nil)
	ctx := context.Background()
	if lit, err := HumanActiveRoomNames(ctx); err != nil || !lit["a"] {
		t.Fatalf("the sidebar's dots %v (%v), want a lit", lit, err)
	}
	// The call ends, and its webhook hasn't come.
	asked.set(map[string][]*livekit.ParticipantInfo{})
	for try := 1; try <= 2; try++ {
		if active, err := IsCallActive(ctx, "a"); err != nil || active {
			t.Errorf("try %d: the ended call is going on %v (%v), want over", try, active, err)
		}
		if exists, err := CheckRoomExists(ctx, "a"); err != nil || exists {
			t.Errorf("try %d: the ended call's room is there %v (%v), want gone", try, exists, err)
		}
	}
	want := map[string]int{"ListRooms": 3, "ListParticipants a": 3}
	if got := asked.counts(); !maps.Equal(got, want) {
		t.Errorf("a sidebar load and two joins asked LiveKit %v; want %v, each join asking", got, want)
	}
}

// A call event empties the kept answer (CallRoomsChanged, which the webhook
// calls for room_finished, participant_joined and participant_left once the
// room is found ours: tests/integration TestACallEventEmptiesTheRoomsCache
// sends each). A read under way when one comes serves its own load and isn't
// kept: it may have been answered before the change.
func TestCallRoomsChangedEmptiesTheRoomsCache(t *testing.T) {
	asked := fakeRooms(t, map[string][]*livekit.ParticipantInfo{"a": {aPerson}})
	roomsOfCalls(t, nil)
	ctx := context.Background()
	dots := func(want string, when string) {
		t.Helper()
		lit, err := HumanActiveRoomNames(ctx)
		if err != nil || len(lit) != 1 || !lit[want] {
			t.Fatalf("%s: the sidebar's dots %v (%v), want %s lit alone", when, lit, err, want)
		}
	}
	dots("a", "a call in a")
	asked.set(map[string][]*livekit.ParticipantInfo{"b": {aPerson}})
	dots("a", "the call moved to b, its event not here yet: the answer kept")
	CallRoomsChanged()
	dots("b", "once its event is here")

	asked.set(map[string][]*livekit.ParticipantInfo{"c": {aPerson}})
	CallRoomsChanged()
	asked.whileAsked("ListRooms", CallRoomsChanged)
	dots("c", "an event while LiveKit was being asked")
	asked.whileAsked("ListRooms", nil)
	dots("c", "the load after it")
	if got := asked.counts()["ListRooms"]; got != 4 {
		t.Errorf("LiveKit was asked for its rooms %d times, want 4: a load, one after the event, the one the next event overtook and the one after it", got)
	}
}
