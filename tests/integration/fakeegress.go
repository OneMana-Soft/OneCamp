//go:build integration
// +build integration

package integration

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"google.golang.org/protobuf/proto"

	"github.com/akashc777/OneCamp/initializers/livekitInit"
)

// FakeEgress stands in for LiveKit's egress service, which a call server in
// dev mode doesn't run. It serves the egress API itself (livekit.Egress over
// Twirp), so the server's own recording code starts, lists and stops room
// recordings through it as it does in production. A recording records
// nothing: it runs until the test ends it (End), as a real one ends when it is
// stopped or its room closes.
type FakeEgress struct {
	// The calls a test never makes answer with a panic, which the egress
	// API reports to the caller as an internal error.
	livekit.Egress

	mu  sync.Mutex
	all []*livekit.EgressInfo
}

// UseFakeEgress points the server's recordings at a FakeEgress for one test.
// The rooms stay on the real call server.
func (env *LiveKitEnv) UseFakeEgress(t *testing.T) *FakeEgress {
	t.Helper()
	f := &FakeEgress{}
	srv := httptest.NewServer(livekit.NewEgressServer(f))
	saved := livekitInit.LiveKitService.EgressClient
	livekitInit.LiveKitService.EgressClient = lksdk.NewEgressClient(srv.URL, env.Key, env.Secret)
	t.Cleanup(func() {
		livekitInit.LiveKitService.EgressClient = saved
		srv.Close()
	})
	return f
}

// StartRoomCompositeEgress starts recording a room that is there now, tagged
// with that room's sid as LiveKit's egress tags it.
func (f *FakeEgress) StartRoomCompositeEgress(ctx context.Context, req *livekit.RoomCompositeEgressRequest) (*livekit.EgressInfo, error) {
	room, err := livekitInit.LiveKitService.GetRoom(ctx, req.RoomName)
	if err != nil {
		return nil, fmt.Errorf("room %s: %w", req.RoomName, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	info := &livekit.EgressInfo{
		EgressId:  "EG_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
		RoomId:    room.Sid,
		RoomName:  req.RoomName,
		Status:    livekit.EgressStatus_EGRESS_ACTIVE,
		StartedAt: time.Now().UnixNano(),
		Request:   &livekit.EgressInfo_RoomComposite{RoomComposite: req},
	}
	f.all = append(f.all, info)
	return proto.Clone(info).(*livekit.EgressInfo), nil
}

// ListEgress lists the recordings of a room, or only those still running
// (starting, active or ending), as LiveKit's does.
func (f *FakeEgress) ListEgress(_ context.Context, req *livekit.ListEgressRequest) (*livekit.ListEgressResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &livekit.ListEgressResponse{}
	for _, e := range f.all {
		if (req.RoomName != "" && e.RoomName != req.RoomName) || (req.EgressId != "" && e.EgressId != req.EgressId) || (req.Active && !running(e)) {
			continue
		}
		out.Items = append(out.Items, proto.Clone(e).(*livekit.EgressInfo))
	}
	return out, nil
}

// StopEgress stops a running recording: it is ending until the test ends it.
func (f *FakeEgress) StopEgress(_ context.Context, req *livekit.StopEgressRequest) (*livekit.EgressInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.all {
		if e.EgressId != req.EgressId {
			continue
		}
		if !running(e) {
			return nil, fmt.Errorf("egress %s is not running", e.EgressId)
		}
		e.Status = livekit.EgressStatus_EGRESS_ENDING
		return proto.Clone(e).(*livekit.EgressInfo), nil
	}
	return nil, fmt.Errorf("no egress %s", req.EgressId)
}

// End ends a recording as LiveKit's egress ends one that was stopped or whose
// room closed: complete, with its file written. It answers what LiveKit's
// egress_ended webhook carries.
func (f *FakeEgress) End(t *testing.T, egressID string) *livekit.EgressInfo {
	t.Helper()
	return f.EndAs(t, egressID, func(e *livekit.EgressInfo) {
		e.SetComplete()
		e.FileResults = []*livekit.FileInfo{{Filename: e.EgressId + ".mp4", Size: 1 << 20, Duration: e.EndedAt - e.StartedAt}}
	})
}

// EndAs ends a recording the way how says, which applies LiveKit's own end to
// it (livekit.EgressInfo's SetComplete, SetAborted or SetFailed, as LiveKit's
// egress calls them, plus any file it wrote). It answers what LiveKit's
// egress_ended webhook carries.
func (f *FakeEgress) EndAs(t *testing.T, egressID string, how func(*livekit.EgressInfo)) *livekit.EgressInfo {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.all {
		if e.EgressId == egressID {
			how(e)
			return proto.Clone(e).(*livekit.EgressInfo)
		}
	}
	t.Fatalf("no recording %s to end", egressID)
	return nil
}

// Started answers the recordings started in a room, oldest first.
func (f *FakeEgress) Started(room string) []*livekit.EgressInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*livekit.EgressInfo
	for _, e := range f.all {
		if e.RoomName == room {
			out = append(out, proto.Clone(e).(*livekit.EgressInfo))
		}
	}
	return out
}

// running is a recording that hasn't ended: starting, active or ending. Pure.
func running(e *livekit.EgressInfo) bool {
	switch e.Status {
	case livekit.EgressStatus_EGRESS_STARTING, livekit.EgressStatus_EGRESS_ACTIVE, livekit.EgressStatus_EGRESS_ENDING:
		return true
	}
	return false
}
