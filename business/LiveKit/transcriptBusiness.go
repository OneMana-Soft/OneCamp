package business

import (
	"context"
	"errors"
	"strings"
	"time"

	transcriptionBusiness "github.com/akashc777/OneCamp/business/Transcription"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// CallLine is one final line of speech, from whichever transcriber heard it.
type CallLine struct {
	RoomName string
	// Identity is the speaker's LiveKit identity. Never what a client says about
	// somebody else: the browser path uses the caller's own session, the agent
	// a participant whose audio it is transcribing. Here it must still be
	// someone connected to the call who is a workspace member.
	Identity  string
	Text      string
	Timestamp int64
	OffsetMs  *int64
	// FromServer is the server's transcriber (the agent), as opposed to a
	// browser posting its own person's words.
	FromServer bool
}

// speakers remembers identities already confirmed as workspace members, so the
// check costs one read per speaker rather than one per line.
var speakers = helpers.NewTTLCache[bool](10 * time.Minute)

// errNoTranscriptHome: the call's session node could not be found after it
// was made, so the line was not kept anywhere rather than somewhere wrong.
var errNoTranscriptHome = errors.New("the call's transcript has nowhere to go")

// ErrNotASpeaker: the identity is not a workspace member's.
var ErrNotASpeaker = errors.New("the speaker is not a member of this workspace")

// isWorkspaceSpeaker reports whether a call identity is a member's Dgraph uid.
// The line becomes an edge to that node, and a node id taken on trust is how a
// writer once turned arbitrary nodes into "users".
func isWorkspaceSpeaker(ctx context.Context, identity string) bool {
	if ok, hit := speakers.Get(identity); hit {
		return ok
	}
	u, err := activeMember(ctx, identity)
	if err != nil {
		return false
	}
	speakers.Set(identity, u != nil)
	return u != nil
}

// StoreCallLine files a final transcript line under the call it was spoken in,
// if and only if that call is being transcribed and keeps a transcript.
//
// Where it goes: the recording running in the call, so the player's transcript
// lines up with the video; otherwise the call's own session node, which is
// linked to its conversation, listed with its recordings, deletable by its
// people and reached by retention.
func StoreCallLine(ctx context.Context, line CallLine) error {
	line.RoomName = strings.TrimSpace(line.RoomName)
	line.Identity = strings.TrimSpace(line.Identity)
	if line.RoomName == "" || line.Identity == "" || strings.TrimSpace(line.Text) == "" {
		return ErrNotInCall
	}

	mode := transcriptionBusiness.Mode()
	if mode == transcriptionBusiness.ModeOff {
		return ErrTranscriptionDisabled
	}
	// One transcriber per mode, or every line is filed twice.
	if line.FromServer != (mode == transcriptionBusiness.ModeBackend) {
		return ErrNotBrowserMode
	}

	// In the call now (cached briefly), and which call this is.
	sessionKey, err := ResolveTranscriptSession(ctx, line.RoomName, line.Identity)
	if err != nil {
		return ErrNotInCall
	}
	sw, recordingEgress := CallSwitches(ctx, line.RoomName, helpers.CallSessionID(sessionKey))
	if !sw.On {
		return ErrTranscriptionOff
	}
	if !sw.Kept {
		return ErrTranscriptNotKept
	}
	if !isWorkspaceSpeaker(ctx, line.Identity) {
		return ErrNotASpeaker
	}

	ts := line.Timestamp
	transcript := &dgraphStruct.DgraphTranscript{
		Text:      line.Text,
		TimeStamp: &ts,
		OffsetMs:  line.OffsetMs,
		From:      &dgraphStruct.DgraphUser{Uid: line.Identity},
		DType:     []string{"Recording"},
	}

	// While a recording runs, its node: so the player's transcript lines up.
	// A recording whose node is still being made (it is written just after
	// the egress starts) gets nothing; the line goes to the call's own
	// session instead of making a node of its own.
	if recordingEgress != "" {
		attached, err := recordingDomain.AttachTranscriptLine(ctx, recordingEgress, transcript)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/StoreCallLine Failed to save transcript err: %+v", err)
			return err
		}
		if attached {
			return nil
		}
	}

	// The call's session node, linked to its conversation, made first when
	// this is the call's first line.
	if err := EnsureCallSessionNode(ctx, sessionKey, line.RoomName); err != nil {
		helpers.LogErrorWithContext(ctx, "business/StoreCallLine Failed to prepare the call session err: %+v", err)
		return err
	}
	attached, err := recordingDomain.AttachTranscriptLine(ctx, sessionKey, transcript)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/StoreCallLine Failed to save transcript err: %+v", err)
		return err
	}
	if !attached {
		return errNoTranscriptHome
	}
	return nil
}
