package domain

// The reads and writes behind permanent removal of recordings: archived ones
// past their purge date, and one someone deletes. See business/Archive/purge.go
// for the rule; this file only knows the graph.

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
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
	return toPurge(recs), nil
}

// RecordingsToPurge returns what a purge needs to know about the recording
// with this egress id, to remove it now: each node holding the id (the
// recording, and any stray node an old archive or delete made for it, which
// has no file and goes too).
func RecordingsToPurge(ctx context.Context, egressId string) ([]ArchivedRecording, error) {
	query := `query Recording($id: string){
		recordingInfo(func: eq(recording_egress_id, $id)) {
			uid
			recording_egress_id
			recording_obj_key
			recording_size
			recording_transcript { uid }
		}
	}`
	recs, err := dgraphModels.GetDgraphRecordingsList(ctx, query, map[string]string{"$id": egressId})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/RecordingsToPurge failed: %v", err)
		return nil, err
	}
	return toPurge(recs), nil
}

func toPurge(recs []*dgraphStruct.DgraphRecording) []ArchivedRecording {
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
	return out
}

// What a recording's node and a transcript line's node hold: the Recording
// and Transcript types in the schema (recordingPredicates_test.go keeps them
// in step).
var (
	recordingPredicates = []string{
		"recording_egress_id", "recording_stared_at", "recording_ended_at", "recording_duration",
		"recording_obj_key", "recording_transcript", "recording_started_by", "recording_channel",
		"recording_dm", "recording_size", "recording_deleted_at", "recording_transcript_only",
		"dgraph.type",
	}
	transcriptPredicates = []string{
		"transcript_from", "transcript_text", "transcript_timestamp", "transcript_offset_ms",
		"dgraph.type",
	}
)

// DeleteRecordingNodes removes a recording and its transcript from the graph.
// Called once the bytes are gone; a node without its file is a broken link.
//
// Each node's predicates are named. A node given as {"uid": x} alone loses
// only what its dgraph.type lists, and transcript lines are written typed
// "Recording", which lists none of theirs: every purge left the transcript's
// text in the graph.
func DeleteRecordingNodes(ctx context.Context, rec ArchivedRecording) error {
	del := []map[string]any{everything(rec.Uid, recordingPredicates)}
	for _, u := range rec.TranscriptUids {
		del = append(del, everything(u, transcriptPredicates))
	}
	raw, err := json.Marshal(del)
	if err != nil {
		return err
	}
	return postDgraphModels.DeleteNodeOrEdges(ctx, string(raw))
}

// everything is a delete of every value of each predicate on the node (a
// JSON null in a delete means all of them).
func everything(uid string, predicates []string) map[string]any {
	node := map[string]any{"uid": uid}
	for _, p := range predicates {
		node[p] = nil
	}
	return node
}
