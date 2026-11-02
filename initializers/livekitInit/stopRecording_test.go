package livekitInit

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	livekit "github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// egressListing is a call server's egress service that only lists: none of
// its recordings runs, or it can't list them at all.
type egressListing struct {
	livekit.Egress // a stop with nothing running asks nothing else
	err            error
}

func (e egressListing) ListEgress(context.Context, *livekit.ListEgressRequest) (*livekit.ListEgressResponse, error) {
	if e.err != nil {
		return nil, e.err
	}
	return &livekit.ListEgressResponse{}, nil
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
			srv := httptest.NewServer(livekit.NewEgressServer(egressListing{err: tc.listErr}))
			t.Cleanup(srv.Close)
			s := &LiveKitServiceStruct{EgressClient: lksdk.NewEgressClient(srv.URL, "test-key", "a-test-secret-long-enough-to-sign-with")}
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
