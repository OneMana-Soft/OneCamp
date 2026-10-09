package business

import (
	"context"
	"time"

	archiveBusiness "github.com/akashc777/OneCamp/business/Archive"
	domain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func AddTranscriptToRecording(ctx context.Context, transScriptInfo *dgraphStruct.DgraphTranscript, egressId string) (err error) {

	dgraphRecordingInfo := &dgraphStruct.DgraphRecording{
		Uid:        "uid(recording)",
		EgressId:   egressId,
		Transcript: []*dgraphStruct.DgraphTranscript{transScriptInfo},
	}

	_, err = domain.CreateOrUpdateDgraphRecording(ctx, dgraphRecordingInfo)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"business/AddTranscriptToRecording Failed to add transcript to recording err: %+v",
			err)
		return

	}

	return
}

func GetDgraphChannelRecordingInfoByEgressId(ctx context.Context, egressId string, userDgraphUID string) (dgraphRecording *dgraphStruct.DgraphRecording, err error) {

	dgraphRecording, err = domain.GetDgraphChannelRecordingInfoByEgressId(ctx, egressId, userDgraphUID)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphChannelRecordingInfoByEgressId Failed to get recording dgraph info err: %+v",
			err)
		return

	}

	return
}

func GetDgraphDmRecordingInfoByEgressId(ctx context.Context, egressId string, userDgraphUID string) (dgraphRecording *dgraphStruct.DgraphRecording, err error) {

	dgraphRecording, err = domain.GetDgraphDmRecordingInfoByEgressId(ctx, egressId, userDgraphUID)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphDmRecordingInfoByEgressId Failed to get recording dgraph info err: %+v",
			err)
		return

	}

	return
}

func UpdateStopRecordingTime(ctx context.Context, egressId string, duration float64, size int64, startedAt int64) (err error) {

	currentTime := time.Now()

	// Create time from epoch (nanoseconds)
	startedAtTime := time.Unix(0, startedAt)

	dgraphRecordingInfo := &dgraphStruct.DgraphRecording{
		Uid:           "uid(recording)",
		EgressId:      egressId,
		EndedAt:       &currentTime,
		StartedAt:     &startedAtTime,
		Duration:      duration,
		RecordingSize: size,
	}

	_, err = domain.CreateOrUpdateDgraphRecording(ctx, dgraphRecordingInfo)

	if err != nil {

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
