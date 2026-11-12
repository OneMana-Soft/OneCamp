package livekitInit

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	livekit "github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// egressListing is a call server's egress service that only lists: the
// recordings running, or it can't list them at all.
type egressListing struct {
	livekit.Egress // a stop with nothing running asks nothing else
	err            error
	running        []*livekit.EgressInfo
}

func (e egressListing) ListEgress(context.Context, *livekit.ListEgressRequest) (*livekit.ListEgressResponse, error) {
	if e.err != nil {
		return nil, e.err
	}
	return &livekit.ListEgressResponse{Items: e.running}, nil
}

// roomKept is a call server's room service holding one room ("ch-1") and the
// metadata written to it last; no room at all when nil.
type roomKept struct {
	livekit.RoomService
	mu   sync.Mutex
	room *livekit.Room
}

func (r *roomKept) ListRooms(context.Context, *livekit.ListRoomsRequest) (*livekit.ListRoomsResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.room == nil {
		return &livekit.ListRoomsResponse{}, nil
	}
	return &livekit.ListRoomsResponse{Rooms: []*livekit.Room{{Name: r.room.Name, Metadata: r.room.Metadata}}}, nil
}

func (r *roomKept) UpdateRoomMetadata(_ context.Context, req *livekit.UpdateRoomMetadataRequest) (*livekit.Room, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.room.Metadata = req.Metadata
	return &livekit.Room{Name: r.room.Name, Metadata: r.room.Metadata}, nil
}

// callServer is a call server with that egress service and a room "ch-1"
// whose metadata is metadata (no room for "-"), and the room as kept.
func callServer(t *testing.T, egress egressListing, metadata string) (*LiveKitServiceStruct, *roomKept) {
	t.Helper()
	const key, secret = "test-key", "a-test-secret-long-enough-to-sign-with"
	kept := &roomKept{}
	if metadata != "-" {
		kept.room = &livekit.Room{Name: "ch-1", Metadata: metadata}
	}
	es := httptest.NewServer(livekit.NewEgressServer(egress))
	rs := httptest.NewServer(livekit.NewRoomServiceServer(kept))
	t.Cleanup(es.Close)
	t.Cleanup(rs.Close)
	return &LiveKitServiceStruct{EgressClient: lksdk.NewEgressClient(es.URL, key, secret), LiveKitClient: lksdk.NewRoomServiceClient(rs.URL, key, secret)}, kept
}

// A stop that finds nothing recording the room says so (ErrNoActiveRecording),
// which every route answers as the asker's view of the call being behind. A
// call server that can't list its recordings is not that: it stays the
// server's failure.
func TestAStopFindsNothingRecordingOnlyWhenNothingIs(t *testing.T) {
	for name, tc := range map[string]struct {
		listErr error
		nothing bool
	}{
		"nothing records the room":                  {nil, true},
		"the call server can't list its recordings": {errors.New("egress is down"), false},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := callServer(t, egressListing{err: tc.listErr}, `{"isRecording":false}`)
			_, err := s.StopRecording(context.Background(), "ch-1")
			if err == nil {
				t.Fatal("a stop with nothing running succeeded")
			}
			if got := errors.Is(err, ErrNoActiveRecording); got != tc.nothing {
				t.Errorf("errors.Is(%q, ErrNoActiveRecording) = %v, want %v", err, got, tc.nothing)
			}
		})
	}
}

// A room that says it records with nothing running (its egress failed or was
// aborted on its own, its start was refused) stops saying so (V-630): on a
// stop, which then is done, and on the end of the egress it names, or of any
// while nothing runs. A room that names a running recording keeps it, and
// every other key of its metadata stays.
func TestAStaleRecordingComesOffItsRoom(t *testing.T) {
	const says = `{"isRecording":true,"recordingStartedBy":"Asha","egressID":"EG_1","transcription":{"on":true}}`
	running := []*livekit.EgressInfo{{EgressId: "EG_2", RoomName: "ch-1", Status: livekit.EgressStatus_EGRESS_ACTIVE}}
	for name, tc := range map[string]struct {
		metadata, ended string
		running         []*livekit.EgressInfo
		cleared         bool
	}{
		"the egress it names ended":              {says, "EG_1", nil, true},
		"it names one, and nothing runs":         {says, "", nil, true},
		"a start's own write, and nothing runs":  {`{"isRecording":true,"recordingStartedBy":"Asha","transcription":{"on":true}}`, "EG_9", nil, true},
		"another egress ended, and its own runs": {says, "EG_0", running, false},
		"it names none, and a recording starts":  {`{"isRecording":true,"recordingStartedBy":"Asha","transcription":{"on":true}}`, "", running, false},
		"it doesn't say it records":              {`{"isRecording":false,"transcription":{"on":true}}`, "EG_1", nil, false},
		"its metadata is unreadable":             {"not json", "EG_1", nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			s, kept := callServer(t, egressListing{running: tc.running}, tc.metadata)
			cleared, err := s.ClearStaleRecording(context.Background(), "ch-1", tc.ended)
			if err != nil || cleared != tc.cleared {
				t.Fatalf("ClearStaleRecording = %v, %v; want %v", cleared, err, tc.cleared)
			}
			on, egress := recordingOf(kept.room.Metadata)
			if tc.cleared && (on || egress != "" || strings.Contains(kept.room.Metadata, "Asha") || !strings.Contains(kept.room.Metadata, "transcription")) {
				t.Errorf("the room's metadata is %s, want no recording and the rest kept", kept.room.Metadata)
			}
			if !tc.cleared && kept.room.Metadata != tc.metadata {
				t.Errorf("the room's metadata is %s, want it as it was: %s", kept.room.Metadata, tc.metadata)
			}
		})
	}

	t.Run("a stop of a room that says it records with nothing running", func(t *testing.T) {
		s, kept := callServer(t, egressListing{}, says)
		if _, err := s.StopRecording(context.Background(), "ch-1"); err != nil {
			t.Fatalf("the stop answered %v, want done", err)
		}
		if on, _ := recordingOf(kept.room.Metadata); on {
			t.Errorf("the room still says it records: %s", kept.room.Metadata)
		}
	})

	t.Run("a stop of a room that is gone", func(t *testing.T) {
		s, _ := callServer(t, egressListing{}, "-")
		if _, err := s.StopRecording(context.Background(), "ch-1"); !errors.Is(err, ErrNoActiveRecording) {
			t.Errorf("the stop answered %v, want ErrNoActiveRecording", err)
		}
	})
}
