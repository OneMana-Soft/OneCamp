package business

// A call that nobody recorded still produces a transcript.
//
// Transcript lines hang off a recording node, and that node is what links them
// to a channel or DM, which is how every reader finds them. Without a recording
// there was no node, so there was nowhere to put the words, so the agent did not
// send them and the API would have refused them. That is the whole reason the
// meeting recap only ever worked for recorded calls.
//
// This creates the missing node for a call session and links it to the surface
// the call belongs to. It is the same node type a recording uses, flagged so
// that no list of playable recordings ever shows it.

import (
	"context"
	"sync"
	"time"

	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// linkedSessions remembers which session nodes have already been attached to
// their surface, so the linking runs once per call instead of once per
// utterance.
//
// A cache rather than a check, because the check IS the expensive part: the
// alternative is a read before every line to ask a question whose answer only
// changes once. Losing it on restart costs one redundant upsert per live call,
// and the upsert is idempotent, so the failure mode of being wrong is a wasted
// write rather than a duplicate node.
var linkedSessions sync.Map

// EnsureCallSessionNode makes sure a transcript for an unrecorded call has
// somewhere to live, and that readers can find it.
//
// A no-op for a real recording, which already has a node created and linked
// when recording started. Refused for a room that belongs to no channel or DM
// (an instant meeting): its lines once landed on a node linked to nothing,
// which no list showed, nobody could delete and retention never reached.
func EnsureCallSessionNode(ctx context.Context, sessionKey, roomName string) error {
	if !helpers.IsCallSessionKey(sessionKey) {
		return nil
	}
	if _, done := linkedSessions.Load(sessionKey); done {
		return nil
	}

	channelUUID, chatGroupID := helpers.ClassifyRoom(roomName)
	if channelUUID == "" && chatGroupID == "" {
		return ErrTranscriptNotKept
	}

	recordingUID, linked, err := recordingDomain.FindCallSessionNode(ctx, sessionKey)
	if err != nil {
		return err
	}
	if linked {
		linkedSessions.Store(sessionKey, struct{}{})
		return nil
	}
	if recordingUID == "" {
		now := time.Now()
		// StartedAt is not decoration: lists order recordings by it, and Dgraph
		// drops nodes that lack the sort predicate. The room's end replaces it
		// with when the call began (EndCallSession).
		recordingUID, err = recordingDomain.CreateOrUpdateDgraphRecording(ctx, &dgraphStruct.DgraphRecording{
			Uid:            "uid(recording)",
			EgressId:       sessionKey,
			StartedAt:      &now,
			TranscriptOnly: true,
		})
		if err != nil {
			return err
		}
		if recordingUID == "" {
			// Made by a racing writer between the read and this write: link
			// whatever is there now.
			if recordingUID, _, err = recordingDomain.FindCallSessionNode(ctx, sessionKey); err != nil || recordingUID == "" {
				return err
			}
		}
	}
	// A node that exists unlinked (an earlier link failed) is linked now. The
	// old code only ever linked a node it had just made, so one failed link left
	// a transcript nobody could find, for ever.

	if channelUUID != "" {
		_, err = channelDomain.CreateOrUpdateDgraphChannel(ctx, &dgraphStruct.DgraphChannel{
			Uid:  "uid(ch)",
			Uuid: channelUUID,
			Recordings: []*dgraphStruct.DgraphRecording{{
				Uid:     recordingUID,
				Channel: &dgraphStruct.DgraphChannel{Uid: "uid(ch)"},
			}},
		})
	} else {
		_, _, err = chatDomain.CreateDM(ctx, &dgraphStruct.DgraphDm{
			Uid: "uid(dm)",
			Recordings: []*dgraphStruct.DgraphRecording{{
				Uid: recordingUID,
				Dm:  &dgraphStruct.DgraphDm{Uid: "uid(dm)"},
			}},
		}, chatGroupID)
	}
	if err != nil {
		return err
	}

	linkedSessions.Store(sessionKey, struct{}{})
	return nil
}

// EndCallSession stamps a finished call's transcript (when it has one) as
// over, from when the room began to now, so it is listed with the
// conversation's recordings, its people can delete it, and the retention
// policy reaches it.
func EndCallSession(ctx context.Context, sessionKey string, began time.Time) {
	if !helpers.IsCallSessionKey(sessionKey) {
		return
	}
	if err := recordingDomain.EndCallSessionNode(ctx, sessionKey, began, time.Now()); err != nil {
		helpers.LogErrorWithContext(ctx, "business/EndCallSession could not end the call's transcript: %+v", err)
	}
	linkedSessions.Delete(sessionKey)
}
