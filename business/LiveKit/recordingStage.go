package business

// A stage page that fails early is recorded again on LiveKit's grid (meet-17).
// A call's recording draws the web's layout page when the web has an origin
// the recorder can reach (livekitInit.RecordingLayoutURL). When that page
// can't be drawn (it is missing, or the recorder's Chrome can't reach it),
// the recorder fails in its first seconds, or LiveKit aborts it because the
// page never said it started; the call is then recorded again, once, on
// LiveKit's grid, and the same recording goes on under the new egress, so
// nobody is shown a failed or empty one.
//
// It keeps no state of its own. A grid recording is never a stage page's
// (livekitInit.IsStageRecording reads the request the webhook carries), so it
// is never recorded again, and nothing has to remember a retry.
//
// Permission: it only follows an egress this server started (its recording
// runs here), in a room whose metadata still names it as recording, while the
// call is still kept recorded: a person's Stop is never undone. Who started
// the recording stays its starter.

import (
	"context"
	"errors"
	"time"

	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/livekit/protocol/livekit"
)

// stageGrace is how soon a stage page's recording must fail to be recorded
// again on the grid. A page that is missing or out of the recorder's reach
// fails in its first seconds; a recording that ran longer and then failed
// (out of memory, storage refused the upload) is a fault, said as one.
const stageGrace = 30 * time.Second

// stageUnrecorded reports whether an ended egress is a stage page's recording
// that may have recorded nothing of the stage: it failed (stageFailedEarly
// asks how soon), or LiveKit aborted it because the page never said it
// started (livekit.MsgStartNotReceived). Never one a person stopped before it
// started, one that reached a limit, one that ended well, or a grid
// recording. Pure.
func stageUnrecorded(info *livekit.EgressInfo) bool {
	if !livekitInit.IsStageRecording(info) {
		return false
	}
	switch info.GetStatus() {
	case livekit.EgressStatus_EGRESS_FAILED:
		return true
	case livekit.EgressStatus_EGRESS_ABORTED:
		return info.GetError() == livekit.MsgStartNotReceived
	}
	return false
}

// stageFailedEarly reports whether an ended egress is a stage page's
// recording that recorded nothing of the stage (stageUnrecorded): aborted
// because the page never said it started, however long LiveKit waited, or
// failed within stageGrace of its start: LiveKit's start, or the recording's
// own (began) when LiveKit gave it none because it never started. Its end is
// LiveKit's, or now when it gave none. Pure.
func stageFailedEarly(info *livekit.EgressInfo, began, now time.Time) bool {
	if !stageUnrecorded(info) {
		return false
	}
	if info.GetStatus() == livekit.EgressStatus_EGRESS_ABORTED {
		return true
	}
	start, end := began, now
	if info.GetStartedAt() > 0 {
		start = time.Unix(0, info.GetStartedAt())
	}
	if info.GetEndedAt() > 0 {
		end = time.Unix(0, info.GetEndedAt())
	}
	return !start.IsZero() && end.Sub(start) <= stageGrace
}

// RecordAgainOnGrid records a call again on LiveKit's grid when its stage
// page's recording failed early (stageFailedEarly), and answers whether it
// did. The same recording goes on under the new egress: its node (its home,
// who started it, its transcript lines), the call's record of it, the call's
// running recording and the room's metadata follow the new egress id. Only a
// recording this server started, still running here and in its room, in a
// call still kept recorded, is followed: another server's egress, or one a
// person stopped, isn't. When the grid can't take over, the end is stamped as
// it came (false). The caller keeps two of a room's starts from racing.
func RecordAgainOnGrid(ctx context.Context, info *livekit.EgressInfo) bool {
	// The cheap test first: most ends are no stage page's failure.
	if !stageUnrecorded(info) {
		return false
	}
	failed, roomName := info.GetEgressId(), info.GetRoomName()
	began, ours, err := recordingDomain.RunningRecordingStart(ctx, failed)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RecordAgainOnGrid could not read the recording %s: %+v", failed, err)
		return false
	}
	if !ours || !stageFailedEarly(info, began, time.Now()) {
		return false
	}
	room, err := livekitInit.LiveKitService.GetRoom(ctx, roomName)
	if err != nil {
		return false
	}
	meta := parseRoomMeta(room.Metadata)
	if !meta.IsRecording || (meta.EgressID != "" && meta.EgressID != failed) || !keptRecorded(ctx, roomName) {
		return false
	}
	egress, objectKey, err := livekitInit.LiveKitService.StartGridRecording(ctx, roomName, meta.RecordingStartedBy)
	if err == nil && egress == nil {
		err = errors.New("the call server started no recording")
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RecordAgainOnGrid could not record again on the grid after %s: %+v", failed, err)
		return false
	}
	if err := recordingDomain.MoveRecording(ctx, failed, egress.EgressId, objectKey); err != nil {
		// A recording nobody finds could never be deleted: it stops.
		helpers.LogErrorWithContext(ctx, "business/RecordAgainOnGrid could not move the recording %s onto %s, so it stops: %+v", failed, egress.EgressId, err)
		if _, serr := livekitInit.LiveKitService.StopRecording(ctx, roomName); serr != nil {
			helpers.LogErrorWithContext(ctx, "business/RecordAgainOnGrid could not stop %s: %+v", egress.EgressId, serr)
		}
		return false
	}
	if err := callStateModel.RecordingMoved(ctx, failed, egress.EgressId); err != nil {
		helpers.LogErrorWithContext(ctx, "business/RecordAgainOnGrid could not move %s onto %s on its call's record: %+v", failed, egress.EgressId, err)
	}
	NoteRecording(ctx, roomName, egress.EgressId)
	callChanged(ctx, roomName)
	helpers.LogInfoWithContext(ctx, "business/RecordAgainOnGrid the stage page's recording %s ended %s (%s): recording on the grid as %s",
		failed, info.GetStatus(), livekitInit.EgressReason(info), egress.EgressId)
	return true
}

// keptRecorded reports whether the call in a room is still kept recorded. A
// person's Stop clears who keeps it recorded before anything else
// (StopRecording), so it holds even when the Stop could not take the
// recording off the room's metadata. A room with no call on record has only
// its metadata to go by.
func keptRecorded(ctx context.Context, roomName string) bool {
	open, err := callStateModel.OpenSessionOf(ctx, roomName)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RecordAgainOnGrid could not read the call in %s: %+v", roomName, err)
		return false
	}
	if open == nil {
		return true
	}
	by, _, err := callStateModel.RecordingWanted(ctx, open.ID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RecordAgainOnGrid could not read who keeps the call in %s recorded: %+v", roomName, err)
		return false
	}
	return by != nil
}
