package models

// Dgraph read path for call transcripts.
//
// Transcripts are stored as recording_transcript edges hanging off a
// DgraphRecording node (keyed by egress id, linked to its channel / DM).
// This is the single source of truth — there is no Postgres transcript
// table. The meeting-recap agent and any other consumer resolve a room to
// its most-recent recording and read the transcript edges in chronological
// order.
//
// Room → recording resolution mirrors how the rest of the app keys these
// surfaces (see helpers.GetGroupingId and the call-room naming):
//   - channel call → room name == ch_uuid              (indexed @exact)
//   - 1:1 DM       → room name == dm_grouping_id        ("uuidA uuidB", @term)
//   - group chat   → room name == dm_grouping_id        (32-char hash, @term)
// We run both resolutions in one query and take whichever surface owns a
// recording, newest first — the recap fires on room_finished, so "newest"
// is unambiguously the call that just ended.

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// GetDgraphTranscriptsByRoom executes the caller-built transcript query (see
// domain/LiveKit.buildTranscriptsByRoomQuery) and returns the newest recording's
// transcript edges in chronological order, plus the resolved recording's egress
// id (so a caller can deep-link to the recording). The query + $room variable
// are built in the domain layer; this model only executes and parses. Returns
// (nil, "", nil) — not an error — when the room has no recording or transcript,
// so callers treat "nothing to summarize" as a benign skip.
func GetDgraphTranscriptsByRoom(ctx context.Context, query string, variables map[string]string) ([]*dgraphStruct.DgraphTranscript, string, error) {
	if query == "" {
		return nil, "", nil
	}

	txn := dgraphInit.DgraphClient.NewTxn()
	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphTranscriptsByRoom failed to query transcripts err: %+v", err)
		return nil, "", err
	}

	var parsed struct {
		ChannelRec []struct {
			Recording []*dgraphStruct.DgraphRecording `json:"ch_recording"`
		} `json:"channelRec"`
		DMRec []struct {
			Recording []*dgraphStruct.DgraphRecording `json:"dm_recording"`
		} `json:"dmRec"`
	}
	if err := json.Unmarshal(resp.Json, &parsed); err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphTranscriptsByRoom failed to unmarshal response err: %+v", err)
		return nil, "", err
	}

	// Prefer whichever surface actually owns a recording with transcripts.
	// A room name is unique to one surface, so at most one block is populated;
	// checking both is just defensive.
	for _, c := range parsed.ChannelRec {
		for _, rec := range c.Recording {
			if rec != nil && len(rec.Transcript) > 0 {
				return rec.Transcript, rec.EgressId, nil
			}
		}
	}
	for _, d := range parsed.DMRec {
		for _, rec := range d.Recording {
			if rec != nil && len(rec.Transcript) > 0 {
				return rec.Transcript, rec.EgressId, nil
			}
		}
	}
	return nil, "", nil
}
