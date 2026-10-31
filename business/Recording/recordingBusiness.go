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
	// StatusEmpty is a recording that captured nothing: nothing came
	// through while it ran, so there is nothing to play, and nothing broke.
	StatusEmpty = "empty"
)

// StatusFromEgress is how a recording ended, from its egress.
//   - Failed: the egress broke and wrote no usable file (out of memory on a
//     small box, storage refused the upload).
//   - Empty: it captured nothing. LiveKit's recording page starts on the first
//     picture or sound in its room, and LiveKit aborts an egress stopped before
//     it started: every camera off and every microphone muted, or stopped in
//     its first seconds. The demo's were all "EGRESS_ABORTED: Start signal not
//     received". One that says complete with nothing written holds nothing
//     either.
//
// Pure.
func StatusFromEgress(status livekit.EgressStatus, size int64) string {
	switch status {
	case livekit.EgressStatus_EGRESS_FAILED:
		return StatusFailed
	case livekit.EgressStatus_EGRESS_ABORTED:
		return StatusEmpty
	}
	if size <= 0 {
		return StatusEmpty
	}
	return StatusReady
}

// EndOfEgress is what a recording's egress left when it ended (egress_ended):
// how it ended (StatusFromEgress), and its file's length in seconds and size.
// A recording that captured nothing is 0 s long, however long it waited for a
// picture or a sound. A length is counted from the egress's start only when
// LiveKit gave it one: one that never started has none, and its end alone
// would be decades. Pure.
func EndOfEgress(info *livekit.EgressInfo) (status string, seconds float64, size int64) {
	for _, file := range info.GetFileResults() {
		if file == nil {
			continue
		}
		seconds = float64(file.Duration) / 1e9
		size = file.Size
	}
	status = StatusFromEgress(info.GetStatus(), size)
	switch {
	case status == StatusEmpty:
		seconds = 0
	case seconds == 0 && info.GetStartedAt() > 0 && info.GetEndedAt() > info.GetStartedAt():
		seconds = float64(info.GetEndedAt()-info.GetStartedAt()) / 1e9
	}
	return status, seconds, size
}

// Failed reports whether a recording broke, so there is nothing to play. A
// recording from before statuses were kept has none, and plays. Pure.
func Failed(status string) bool {
	return status == StatusFailed
}

// Empty reports whether a recording captured nothing, so there is nothing to
// play. Pure.
func Empty(status string) bool {
	return status == StatusEmpty
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
