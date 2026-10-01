package business

import (
	adapter "github.com/akashc777/OneCamp/adapter/LiveKit"
	recordingBusiness "github.com/akashc777/OneCamp/business/Recording"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"

	"context"

	"github.com/akashc777/OneCamp/helpers"
)

func SaveTranscript(ctx context.Context, transcriptInfo *adapter.TranscriptInput) (err error) {

	transcriptDgraphInfo := &dgraphStruct.DgraphTranscript{
		Text:      transcriptInfo.Text,
		TimeStamp: &transcriptInfo.Timestamp,
		OffsetMs:  transcriptInfo.OffsetMs,
		From: &dgraphStruct.DgraphUser{
			Uid: transcriptInfo.ParticipantIdentity,
		},
		DType: []string{"Recording"},
	}

	// An unrecorded call has no recording node, so make the session node first
	// or the line has nowhere to attach and nothing to be found through.
	if err = EnsureCallSessionNode(ctx, transcriptInfo.EgressID, transcriptInfo.RoomName); err != nil {
		helpers.LogErrorWithContext(ctx, "business/SaveTranscript Failed to prepare the call session err: %+v", err)
		return
	}

	err = recordingBusiness.AddTranscriptToRecording(ctx, transcriptDgraphInfo, transcriptInfo.EgressID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/SaveTranscript Failed to save transcript err: %+v", err)
		return
	}
	return
}
