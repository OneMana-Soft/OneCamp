package business

import (
	"context"
	"time"

	"github.com/livekit/protocol/livekit"

	archiveBusiness "github.com/akashc777/OneCamp/business/Archive"
	domain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
)

// Recording statuses, from LiveKit's egress status when it ended, in the words
// the lists and the player answer with ("recording_status").
const (
	StatusReady  = "ready"
	StatusFailed = "failed"
)

// StatusFromEgress is how a recording ended, from its egress: a failed or
// aborted egress wrote no usable file (out of memory on a small box, the room
// closed under it, storage refused the upload), and neither did one that says
// complete with nothing written. Pure.
func StatusFromEgress(status livekit.EgressStatus, size int64) string {
	switch status {
	case livekit.EgressStatus_EGRESS_FAILED, livekit.EgressStatus_EGRESS_ABORTED:
		return StatusFailed
	}
	if size <= 0 {
		return StatusFailed
	}
	return StatusReady
}

// Failed reports whether a recording status means there is nothing to play.
// Empty (recorded before the status was kept) is not failed. Pure.
func Failed(status string) bool {
	return status == StatusFailed
}

// UpdateStopRecordingTime stamps a recording's end, its length and size, and
// how it ended.
//
// It names the recording by its egress id and writes nothing when there is
// none here: it used to upsert, and an egress this server never started (a
// shared call server sends every backend every room's events) became a stray
// node holding only an egress id and times.
func UpdateStopRecordingTime(ctx context.Context, egressId string, duration float64, size int64, startedAt int64, status string) (err error) {
	fields := map[string]interface{}{
		"recording_ended_at": time.Now(),
		"recording_duration": duration,
		"recording_size":     size,
		"recording_status":   status,
	}
	if startedAt > 0 {
		fields["recording_stared_at"] = time.Unix(0, startedAt)
	}
	if err = domain.EndRecording(ctx, egressId, fields); err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateStopRecordingTime Failed to update recording end time err: %+v",
			err)
		return
	}
	return
}

// DeleteRecording removes a recording someone deleted, for good: its file in
// storage, then the recording and its transcript, as the purge of archived
// recordings does (archiveBusiness.PurgeRecording).
//
// It was a soft delete that never reached the recording: its mutation named no
// node, so it made a new one (see domain.setRecordingsDeletedAt), and the
// recording stayed listed and playable. Had it worked, the file would still
// have stayed in storage unless the workspace had set a purge.
//
// The caller checks who may delete it: a channel's moderators or a workspace
// admin, or anyone in the conversation.
func DeleteRecording(ctx context.Context, egressId string) error {
	recs, err := recordingsToPurge(ctx, egressId)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if _, err := purgeRecording(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// The seams DeleteRecording's tests replace.
var (
	recordingsToPurge = domain.RecordingsToPurge
	purgeRecording    = archiveBusiness.PurgeRecording
)
