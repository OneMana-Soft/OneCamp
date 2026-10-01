package business

import (
	"context"
	"time"

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

// SoftDeleteRecording soft-deletes a recording by egress ID.
// Requires the caller to verify permissions (channel admin, DM participant, group member).
func SoftDeleteRecording(ctx context.Context, egressId string) error {
	return domain.SoftDeleteDgraphRecording(ctx, egressId)
}
