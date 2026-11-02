package business

// ONE start for every call's recording (meet-02, head D): a channel's, a DM's,
// a group's and a calendar event's call are recorded the same way, and the
// recording is filed where the call keeps what it makes (callHome), so no
// surface keeps its own copy of the start.

import (
	"context"
	"errors"
	"fmt"
	"time"

	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
)

// The start's steps, seams for tests: LiveKit's egress, the recording's node
// (found again by its egress id), filing it under its call's home, and what a
// running recording tells its call.
var (
	startEgress = func(ctx context.Context, room, name string) (*livekit.EgressInfo, string, error) {
		return livekitInit.LiveKitService.StartRecording(ctx, room, name)
	}
	stopEgress = func(ctx context.Context, room string) error {
		_, err := livekitInit.LiveKitService.StopRecording(ctx, room)
		return err
	}
	saveRecordingNode = recordingDomain.CreateOrUpdateDgraphRecording
	findRecordingNode = recordingDomain.FindCallSessionNode
	fileUnder         = fileUnderHome
	recordingRunning  = noteRecordingRunning
)

// StartCallRecording starts recording the call in a room, as user, and files
// the recording where the call keeps what it makes (homeOfCall): its channel,
// its DM or group, or the calendar event whose call it is. Every surface's
// start is this one; who may start it is the caller's rule.
//
// A call that keeps nothing (an instant meeting, a personal room) is refused
// with ErrTranscriptNotKept before anything records, and so is a call whose
// event can't be read just now. A recording that can't be filed is stopped:
// one running where nobody finds it could never be deleted, and the retention
// policy would never reach it.
func StartCallRecording(ctx context.Context, room string, user *dgraphStruct.DgraphUser) error {
	home, err := homeOfCall(ctx, room)
	if err != nil {
		return err
	}
	egress, objectKey, err := startEgress(ctx, room, callName(room, user))
	if err == nil && egress == nil {
		err = errors.New("the call server started no recording")
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/StartCallRecording could not record the call in %s: %+v", room, err)
		return err
	}
	if err := fileRecording(ctx, egress.EgressId, objectKey, user, home); err != nil {
		helpers.LogErrorWithContext(ctx, "business/StartCallRecording could not file the recording of %s, so it stops: %+v", room, err)
		if serr := stopEgress(ctx, room); serr != nil {
			helpers.LogErrorWithContext(ctx, "business/StartCallRecording could not stop the unfiled recording of %s: %+v", room, serr)
		}
		return err
	}
	recordingRunning(ctx, room, egress.EgressId, user)
	return nil
}

// fileRecording makes a running recording's node (its egress id, where its
// file goes, when and by whom it started, not ended) and files it under its
// call's home.
func fileRecording(ctx context.Context, egressID, objectKey string, user *dgraphStruct.DgraphUser, home callHome) error {
	now, notEnded := time.Now(), time.Time{}
	node := &dgraphStruct.DgraphRecording{
		Uid:       "uid(recording)",
		EgressId:  egressID,
		StartedAt: &now,
		EndedAt:   &notEnded,
		ObjectKey: objectKey,
	}
	if user != nil && user.Uid != "" {
		node.RecordingStartedBy = &dgraphStruct.DgraphUser{Uid: user.Uid}
	}
	uid, err := saveRecordingNode(ctx, node)
	if err != nil {
		return err
	}
	if uid == "" {
		// The upsert found a node with its egress id already: file that one.
		if uid, _, err = findRecordingNode(ctx, egressID); err != nil {
			return err
		}
		if uid == "" {
			return fmt.Errorf("the recording %s has no node", egressID)
		}
	}
	return fileUnder(ctx, uid, home)
}

// noteRecordingRunning tells a room's call that a recording runs in it: its
// transcript lines are filed under the recording while it runs, the call's
// record lists it, and whoever started it keeps the call recorded (a recording
// ends with its room, and the call records again as them when its room is made
// again after its breakout rooms: business/CallControl).
func noteRecordingRunning(ctx context.Context, room, egressID string, user *dgraphStruct.DgraphUser) {
	NoteRecording(ctx, room, egressID)
	var by *uuid.UUID
	if user != nil {
		if id, perr := uuid.Parse(user.Uuid); perr == nil {
			by = &id
		}
	}
	if _, ferr := callStateModel.RecordingStarted(ctx, room, egressID, by, time.Now()); ferr != nil {
		helpers.LogErrorWithContext(ctx, "business/StartCallRecording could not file the recording under its call: %+v", ferr)
	}
	if by != nil {
		if kerr := callStateModel.KeepRecording(ctx, room, by); kerr != nil {
			helpers.LogErrorWithContext(ctx, "business/StartCallRecording could not note who keeps the call in %s recorded: %+v", room, kerr)
		}
	}
	callChanged(ctx, room)
}
