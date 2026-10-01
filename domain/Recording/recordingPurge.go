package domain

// The reads and writes behind permanent removal of archived recordings.
// See business/Archive/purge.go for the rule; this file only knows the graph.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	postDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Post"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Recording"
)

// ArchivedRecording is the little a purge needs to know about a recording:
// where its bytes are, how many, and which nodes go with it.
type ArchivedRecording struct {
	Uid            string
	EgressId       string
	ObjectKey      string
	Size           int64
	TranscriptUids []string
}

// ListRecordingsArchivedBefore returns recordings archived before the cutoff,
// oldest first, at most limit of them.
func ListRecordingsArchivedBefore(ctx context.Context, cutoff time.Time, limit int) ([]ArchivedRecording, error) {
	variables := map[string]string{
		"$cutoff": cutoff.Format(time.RFC3339),
		"$first":  strconv.Itoa(limit),
	}
	query := `query Recordings($cutoff: string, $first: int){
		recordingInfo(func: has(recording_egress_id), orderasc: recording_deleted_at, first: $first) @filter(gt(recording_deleted_at, "1970-01-01T00:00:00Z") AND lt(recording_deleted_at, $cutoff)) {
			uid
			recording_egress_id
			recording_obj_key
			recording_size
			recording_transcript { uid }
		}
	}`
	recs, err := dgraphModels.GetDgraphRecordingsList(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ListRecordingsArchivedBefore failed: %v", err)
		return nil, err
	}
	out := make([]ArchivedRecording, 0, len(recs))
	for _, r := range recs {
		a := ArchivedRecording{Uid: r.Uid, EgressId: r.EgressId, ObjectKey: r.ObjectKey, Size: r.RecordingSize}
		for _, t := range r.Transcript {
			if t != nil && t.Uid != "" {
				a.TranscriptUids = append(a.TranscriptUids, t.Uid)
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// DeleteRecordingNodes removes a recording and its transcript from the graph.
// Called once the bytes are gone; a node without its file is a broken link.
func DeleteRecordingNodes(ctx context.Context, rec ArchivedRecording) error {
	uids := append([]string{rec.Uid}, rec.TranscriptUids...)
	var del []string
	for _, u := range uids {
		del = append(del, fmt.Sprintf(`{"uid": %q}`, u))
	}
	return postDgraphModels.DeleteNodeOrEdges(ctx, "["+strings.Join(del, ",")+"]")
}
