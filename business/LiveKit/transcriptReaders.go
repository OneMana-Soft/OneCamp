package business

// Who may read what a call has said (V-860, main 9 Nov 23:4x).
//
// A LIVE call's transcript is for the people connected to that call: they see
// who is there, and nobody else may listen in through the AI. A FINISHED
// call's transcript is for its conversation's members, as the recap already
// shares it with them. Every reader acting for a person asks here, after its
// own check of the conversation: the in-call assistant (ConnectedCall), and
// the readers of a room's newest transcript (NewestTranscriptOpenTo). The
// recap acts for the call itself and asks nothing.
//
// Presence is LiveKit's now (connectedNow), never the grant the write path
// caches (ResolveTranscriptSession): that outlives a leave, a removal and the
// room's call by up to a minute, and here it would be other people's words.

import (
	"context"
	"strings"
	"time"

	transcriptDomain "github.com/akashc777/OneCamp/domain/LiveKit"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
)

// LiveCall is a call going on, as one person connected to it reads it: the
// room its transcript is kept in, the key it is kept under, and when it began.
type LiveCall struct {
	Room, Key string
	StartedAt time.Time
}

// ConnectedCall answers the call going on in room for identity (a LiveKit
// identity: the person's graph uid) when they are connected to it, else
// ErrNotInCall (callState.go), the same answer for a room with no call, so a
// refusal never says whether a call is running. Presence and the key are
// connectedNow's: this call's (its room sid, the key its lines are filed
// under) and this person's, now. When it began is Call State's; until Call
// State has the call (its join webhook lags the join), it began now: its own
// lines, no recording.
func ConnectedCall(ctx context.Context, room, identity string) (*LiveCall, error) {
	room = strings.TrimSpace(room)
	key, err := connectedNow(ctx, room, identity)
	if err != nil {
		return nil, ErrNotInCall
	}
	call := &LiveCall{Room: room, Key: key, StartedAt: time.Now()}
	if open, err := callStateModel.OpenSessionOf(ctx, room); err == nil && open != nil {
		call.StartedAt = open.StartedAt
	}
	return call, nil
}

// Kept is what the call has kept so far, in the order it was said: its own
// lines, and those on recordings begun during it. At most limit lines.
func (c *LiveCall) Kept(ctx context.Context, limit int) ([]transcriptDomain.TranscriptLine, error) {
	lines, _, err := transcriptDomain.GetCallTranscriptLines(ctx, c.Room, c.Key,
		c.StartedAt.Add(-recordingDomain.CallWindowSlack), time.Now().Add(recordingDomain.CallWindowSlack), limit)
	return lines, err
}

// NewestTranscriptOpenTo reports whether identity, whom the room's
// conversation already admits (the caller's own check, made first), may read
// the room's newest transcript now. With no call going on in the room, yes.
// Connected to the call going on, or to one of its breakout rooms, yes.
// Otherwise the newest transcript is the running call's as soon as it, or a
// breakout room of it, has kept a line, and then no; until then it is a
// finished call's, and yes. Unsure is no.
func NewestTranscriptOpenTo(ctx context.Context, room, identity string) bool {
	room = strings.TrimSpace(room)
	open, err := callStateModel.OpenSessionOf(ctx, room)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/NewestTranscriptOpenTo call state err: %+v", err)
		return false
	}
	if open == nil {
		return true
	}
	// A breakout room keeps its transcript where its call does (homeOfCall).
	breakouts, err := callStateModel.OpenRoomsOf(ctx, open.ID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/NewestTranscriptOpenTo breakout rooms err: %+v", err)
		return false
	}
	running := append([]callStateModel.Session{*open}, breakouts...)
	for _, s := range running {
		if _, err := connectedNow(ctx, s.Room, identity); err == nil {
			return true
		}
	}
	for _, s := range running {
		if s.RoomSID == "" {
			return false
		}
		call := &LiveCall{Room: room, Key: helpers.CallSessionKeyFor(s.RoomSID), StartedAt: s.StartedAt}
		if kept, err := call.Kept(ctx, 1); err != nil || len(kept) > 0 {
			return false
		}
	}
	return true
}
