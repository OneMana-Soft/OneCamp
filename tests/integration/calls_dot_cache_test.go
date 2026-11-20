//go:build integration

package integration_test

// H3C (BACKEND-OPT, 19 Nov 2026). The sidebar keeps LiveKit's answer of which
// calls are going on for 3 seconds (business/LiveKit's activeRooms), so the
// loads in that time ask LiveKit nothing. Each call event that changes the
// answer, as LiveKit's webhook brings it to the server's own handler, empties
// it: a call's dot is right on the next load, not 3 seconds on. LiveKit here is
// a fake whose rooms the test changes, so a kept answer and a new one tell
// apart; the rooms are channels of this workspace, so the events are ours.

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"google.golang.org/protobuf/proto"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestACallEventEmptiesTheRoomsCache(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	lk := fakeCallServer(t)

	for _, c := range []struct {
		event         string
		before, after bool
	}{
		{"participant_joined", false, true},
		{"participant_left", true, false},
		{"room_finished", true, false},
	} {
		t.Run(c.event, func(t *testing.T) {
			room := uuid.NewString()
			// Each event's call has a person of its own: a user's email is unique.
			name := "maya" + room[:8]
			people := seedChannel(t, pg, dg, room, name)
			maya := &livekit.ParticipantInfo{Identity: people[name].Uid, Sid: "PA_" + room[:8], State: livekit.ParticipantInfo_ACTIVE}
			in := func(lit bool) map[string][]*livekit.ParticipantInfo {
				if lit {
					return map[string][]*livekit.ParticipantInfo{room: {maya}}
				}
				return map[string][]*livekit.ParticipantInfo{}
			}
			event := func() {
				lkWebhook(t, lk.env, &livekit.WebhookEvent{Event: c.event, Room: &livekit.Room{Name: room, Sid: "RM_" + room[:8]}, Participant: maya})
			}
			// The kept answer's clock stands still from here: the answer kept
			// is served however loaded the machine is, and only the event can
			// empty it, never the 3 s running out.
			t.Cleanup(liveKitBusiness.HoldClockForTest())
			lk.set(in(c.before))
			event() // whatever was kept before this call is gone
			sidebarDotIs(t, ctx, room, c.before, "before the call changed")
			lk.set(in(c.after))
			sidebarDotIs(t, ctx, room, c.before, "the call changed, its event not here yet: the answer kept")
			event()
			sidebarDotIs(t, ctx, room, c.after, "once its event is here")
		})
	}
}

// sidebarDotIs reads a room's dot as the sidebar reads every call's at once.
func sidebarDotIs(t *testing.T, ctx context.Context, room string, want bool, when string) {
	t.Helper()
	lit, err := liveKitBusiness.HumanActiveRoomNames(ctx)
	if err != nil {
		t.Fatalf("%s: the sidebar's dots: %v", when, err)
	}
	if lit[room] != want {
		t.Errorf("%s: the sidebar's dot lit %v, want %v", when, lit[room], want)
	}
}

// fakeCall is a call server whose rooms a test sets: it answers ListRooms and
// ListParticipants from them, and anything else (a removal, a room deleted)
// with an empty answer. The server's webhook handler takes events signed with
// its key.
type fakeCall struct {
	env   *integration.LiveKitEnv
	mu    sync.Mutex
	rooms map[string][]*livekit.ParticipantInfo
}

func (f *fakeCall) set(rooms map[string][]*livekit.ParticipantInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms = rooms
}

func (f *fakeCall) now() map[string][]*livekit.ParticipantInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.rooms)
}

func fakeCallServer(t *testing.T) *fakeCall {
	t.Helper()
	const key, secret = "test-key", "test-secret-long-enough-to-sign-with"
	f := &fakeCall{env: &integration.LiveKitEnv{Key: key, Secret: secret}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rooms := f.now()
		var out proto.Message = &livekit.Room{}
		switch path.Base(r.URL.Path) {
		case "ListRooms":
			var req livekit.ListRoomsRequest
			_ = proto.Unmarshal(body, &req)
			names := req.Names
			if len(names) == 0 {
				names = slices.Sorted(maps.Keys(rooms))
			}
			resp := &livekit.ListRoomsResponse{}
			for _, name := range names {
				if in, ok := rooms[name]; ok {
					resp.Rooms = append(resp.Rooms, &livekit.Room{Name: name, Sid: "RM_" + name[:8], NumParticipants: uint32(len(in))})
				}
			}
			out = resp
		case "ListParticipants":
			var req livekit.ListParticipantsRequest
			_ = proto.Unmarshal(body, &req)
			in, ok := rooms[req.Room]
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"not_found","msg":"requested room does not exist"}`))
				return
			}
			out = &livekit.ListParticipantsResponse{Participants: in}
		}
		w.Header().Set("Content-Type", "application/protobuf")
		b, _ := proto.Marshal(out)
		_, _ = w.Write(b)
	}))
	t.Cleanup(server.Close)
	f.env.URL = server.URL
	saved := livekitInit.LiveKitService
	t.Cleanup(func() { livekitInit.LiveKitService = saved })
	// Nothing this server answered is kept for the next test's.
	t.Cleanup(liveKitBusiness.CallRoomsChanged)
	livekitInit.LiveKitService = livekitInit.LiveKitServiceStruct{
		LiveKitClient: lksdk.NewRoomServiceClient(server.URL, key, secret),
		Config:        &livekitInit.LiveKitConfigStruct{HostURL: server.URL, ApiKey: key, ApiSecret: secret},
	}
	return f
}
