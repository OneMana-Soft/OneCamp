// Package livekitfake stands in for LiveKit's room service in tests that ask
// who is in a call: rooms, each with its session sid and the people connected
// to it, which the test changes as it goes (someone leaves, a host puts
// someone out, the room's next call begins under a new sid).
package livekitfake

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"sync"
	"testing"

	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"google.golang.org/protobuf/proto"
)

// Fake is the room service a test talks to through the project's LiveKit
// service. Its rooms change only through its methods and the calls LiveKit
// would answer (RemoveParticipant).
type Fake struct {
	mu    sync.Mutex
	rooms map[string]*fakeRoom
}

type fakeRoom struct {
	sid    string
	people []string
}

// Serve points the project's LiveKit service, for the test's length, at a
// fake with one room (its session sid) and the people connected to it, by
// identity. Any other room is not there until Room adds it.
func Serve(t testing.TB, room, sid string, participants ...string) *Fake {
	t.Helper()
	f := &Fake{rooms: map[string]*fakeRoom{}}
	f.Room(room, sid, participants...)
	const key, secret = "test-key", "test-secret-long-enough-to-sign-with"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/protobuf")
		b, _ := proto.Marshal(f.answer(path.Base(r.URL.Path), body))
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
	return f
}

// Room makes a room the call under sid, with exactly these people connected:
// a new room, or the room's next call (a new sid), or who is in it now.
func (f *Fake) Room(room, sid string, participants ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms[room] = &fakeRoom{sid: sid, people: slices.Clone(participants)}
}

// Leave disconnects identity from room, as their closing the tab does.
func (f *Fake) Leave(room, identity string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.rooms[room]; r != nil {
		r.people = slices.DeleteFunc(r.people, func(p string) bool { return p == identity })
	}
}

// answer is the room service's reply to one call, by its method name.
func (f *Fake) answer(method string, body []byte) proto.Message {
	switch method {
	case "ListRooms":
		var req livekit.ListRoomsRequest
		_ = proto.Unmarshal(body, &req)
		resp := &livekit.ListRoomsResponse{}
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, name := range req.Names {
			if r := f.rooms[name]; r != nil {
				resp.Rooms = append(resp.Rooms, &livekit.Room{Name: name, Sid: r.sid})
			}
		}
		return resp
	case "ListParticipants":
		var req livekit.ListParticipantsRequest
		_ = proto.Unmarshal(body, &req)
		resp := &livekit.ListParticipantsResponse{}
		f.mu.Lock()
		defer f.mu.Unlock()
		if r := f.rooms[req.Room]; r != nil {
			for _, id := range r.people {
				resp.Participants = append(resp.Participants, &livekit.ParticipantInfo{Identity: id, State: livekit.ParticipantInfo_ACTIVE})
			}
		}
		return resp
	case "RemoveParticipant":
		var req livekit.RoomParticipantIdentity
		_ = proto.Unmarshal(body, &req)
		f.Leave(req.Room, req.Identity)
		return &livekit.RemoveParticipantResponse{}
	default:
		return &livekit.Room{}
	}
}
