package business

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
)

// startSeen is what one start did, through its seams.
type startSeen struct {
	egress, stopped, running, filedUIDs []string
	saved                               []*dgraphStruct.DgraphRecording
	filed                               []callHome
}

// fakeStart puts every step of the start behind a fake for one test: the
// meeting "meet-ev" is the event ev's call, "meet-unread" is a meeting whose
// event can't be read just now, and any other meeting is no event's. Filing
// answers fileErr.
func fakeStart(t *testing.T, ev uuid.UUID, fileErr error) *startSeen {
	t.Helper()
	was := struct {
		start   func(context.Context, string, string) (*livekit.EgressInfo, string, error)
		stop    func(context.Context, string) error
		save    func(context.Context, *dgraphStruct.DgraphRecording) (string, error)
		find    func(context.Context, string) (string, bool, error)
		file    func(context.Context, string, callHome) error
		running func(context.Context, string, string, *dgraphStruct.DgraphUser)
		event   func(context.Context, string) (string, error)
	}{startEgress, stopEgress, saveRecordingNode, findRecordingNode, fileUnder, recordingRunning, callEvent}
	t.Cleanup(func() {
		startEgress, stopEgress, saveRecordingNode, findRecordingNode = was.start, was.stop, was.save, was.find
		fileUnder, recordingRunning, callEvent = was.file, was.running, was.event
	})
	s := &startSeen{}
	callEvent = func(_ context.Context, room string) (string, error) {
		switch room {
		case "meet-ev":
			return ev.String(), nil
		case "meet-unread":
			return "", errors.New("the calendar didn't answer")
		}
		return "", nil
	}
	startEgress = func(_ context.Context, room, _ string) (*livekit.EgressInfo, string, error) {
		s.egress = append(s.egress, room)
		return &livekit.EgressInfo{EgressId: "EG_" + room}, "recordings/" + room + ".mp4", nil
	}
	stopEgress = func(_ context.Context, room string) error {
		s.stopped = append(s.stopped, room)
		return nil
	}
	saveRecordingNode = func(_ context.Context, r *dgraphStruct.DgraphRecording) (string, error) {
		s.saved = append(s.saved, r)
		return "0x7", nil
	}
	findRecordingNode = func(context.Context, string) (string, bool, error) {
		t.Error("a new recording's node was looked for again")
		return "", false, nil
	}
	fileUnder = func(_ context.Context, uid string, h callHome) error {
		s.filedUIDs, s.filed = append(s.filedUIDs, uid), append(s.filed, h)
		return fileErr
	}
	recordingRunning = func(_ context.Context, room, egressID string, _ *dgraphStruct.DgraphUser) {
		s.running = append(s.running, room+" "+egressID)
	}
	return s
}

// A call is recorded where it keeps what it makes, through the ONE start (head
// D): an event's call on its event, a channel's in its channel, a DM's or a
// group's in its conversation. A call that keeps nothing (an instant meeting)
// is refused before anything records, and so is one whose event can't be read
// just now; a recording that can't be filed is stopped, never left running
// where nobody finds it, deletes it or keeps it for the retention policy.
func TestACallIsRecordedWhereItKeepsWhatItMakes(t *testing.T) {
	ctx := context.Background()
	ev := uuid.MustParse("6f1c0a52-8d1e-4a35-9a51-5d8f0a1b2c3d")
	maya := &dgraphStruct.DgraphUser{Uid: "0x2a", Uuid: "0b6e5d4c-3b2a-4190-8f7e-6d5c4b3a2910"}
	channel := "0b9f6a2e-3c4d-4e5f-8a9b-0c1d2e3f4a5b"
	dm := helpers.GetGroupingId(maya.Uuid, "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f")

	for _, c := range []struct {
		name, room string
		home       callHome
	}{
		{"an event's call, on its event", "meet-ev", callHome{event: ev}},
		{"a channel's call, in its channel", channel, callHome{channel: channel}},
		{"a DM's call, in the DM", dm, callHome{chat: dm}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := fakeStart(t, ev, nil)
			before := time.Now()
			if err := StartCallRecording(ctx, c.room, maya); err != nil {
				t.Fatalf("StartCallRecording(%s): %v", c.room, err)
			}
			if !slices.Equal(s.egress, []string{c.room}) {
				t.Fatalf("recordings started: %v, want one in %s", s.egress, c.room)
			}
			if len(s.saved) != 1 {
				t.Fatalf("recording nodes saved: %d, want 1", len(s.saved))
			}
			n := s.saved[0]
			if n.EgressId != "EG_"+c.room || n.ObjectKey != "recordings/"+c.room+".mp4" {
				t.Errorf("the node is egress %q at %q, want the recording's own", n.EgressId, n.ObjectKey)
			}
			if n.RecordingStartedBy == nil || n.RecordingStartedBy.Uid != maya.Uid {
				t.Errorf("the node says it was started by %+v, want %s", n.RecordingStartedBy, maya.Uid)
			}
			if n.StartedAt == nil || n.StartedAt.Before(before) || n.EndedAt == nil || !n.EndedAt.IsZero() || n.TranscriptOnly {
				t.Errorf("the node is started %v, ended %v, transcript-only %v: want started now, not ended, a recording", n.StartedAt, n.EndedAt, n.TranscriptOnly)
			}
			if !slices.Equal(s.filed, []callHome{c.home}) || !slices.Equal(s.filedUIDs, []string{"0x7"}) {
				t.Errorf("filed %v under %+v, want 0x7 under %+v", s.filedUIDs, s.filed, c.home)
			}
			if !slices.Equal(s.running, []string{c.room + " EG_" + c.room}) || len(s.stopped) != 0 {
				t.Errorf("the call was told %v and stopped %v, want told once and nothing stopped", s.running, s.stopped)
			}
		})
	}

	t.Run("an instant meeting keeps nothing, so nothing records", func(t *testing.T) {
		s := fakeStart(t, ev, nil)
		if err := StartCallRecording(ctx, "meet-instant", maya); !errors.Is(err, ErrTranscriptNotKept) {
			t.Fatalf("StartCallRecording(an instant meeting) = %v, want ErrTranscriptNotKept", err)
		}
		if len(s.egress)+len(s.saved)+len(s.filed)+len(s.running) != 0 {
			t.Errorf("an instant meeting's start did: egress %v, nodes %d, filed %v, told %v", s.egress, len(s.saved), s.filed, s.running)
		}
	})

	t.Run("an event that can't be read just now starts nothing", func(t *testing.T) {
		s := fakeStart(t, ev, nil)
		if err := StartCallRecording(ctx, "meet-unread", maya); err == nil || errors.Is(err, ErrTranscriptNotKept) {
			t.Fatalf("StartCallRecording(an unread event's call) = %v, want the read's error", err)
		}
		if len(s.egress) != 0 {
			t.Errorf("recordings started: %v, want none", s.egress)
		}
	})

	t.Run("a recording that can't be filed is stopped", func(t *testing.T) {
		s := fakeStart(t, ev, errors.New("the graph didn't answer"))
		if err := StartCallRecording(ctx, "meet-ev", maya); err == nil {
			t.Fatal("StartCallRecording = nil, want the filing's error")
		}
		if !slices.Equal(s.stopped, []string{"meet-ev"}) {
			t.Errorf("stopped %v, want the recording in meet-ev stopped", s.stopped)
		}
		if len(s.running) != 0 {
			t.Errorf("the call was told a recording runs: %v", s.running)
		}
	})
}
