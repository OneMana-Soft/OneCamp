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
// when recording started, and for a room that belongs to no channel or DM (an
// instant meeting), which has no surface to attach a transcript to and never
// had one.
func EnsureCallSessionNode(ctx context.Context, sessionKey, roomName string) error {
	if !helpers.IsCallSessionKey(sessionKey) {
		return nil
	}
	if _, done := linkedSessions.Load(sessionKey); done {
		return nil
	}

	channelUUID, chatGroupID := helpers.ClassifyRoom(roomName)
	if channelUUID == "" && chatGroupID == "" {
		// An instant meeting: no channel, no DM, nothing that lists calls. Mark
		// it handled so the classification is not redone per utterance.
		linkedSessions.Store(sessionKey, struct{}{})
		return nil
	}

	now := time.Now()
	// StartedAt is not decoration: the transcript read orders recordings by it
	// and takes the newest, and Dgraph drops nodes that lack the sort predicate.
	// A session node without it is a node the recap can never see.
	recordingUID, err := recordingDomain.CreateOrUpdateDgraphRecording(ctx, &dgraphStruct.DgraphRecording{
		Uid:            "uid(recording)",
		EgressId:       sessionKey,
		StartedAt:      &now,
		TranscriptOnly: true,
	})
	if err != nil {
		return err
	}
	if recordingUID == "" {
		return nil
	}

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
