package domain

// Transcript domain — Dgraph-backed.
//
// Transcripts live exclusively in Dgraph as recording_transcript edges on the
// recording node (there is no Postgres transcript table). This domain resolves
// a room to its newest recording and returns the transcript as ordered,
// store-agnostic lines for the meeting-recap agent.

import (
	"context"
	"fmt"
	"github.com/akashc777/OneCamp/helpers"

	recordingDgraph "github.com/akashc777/OneCamp/models/dgraph/Recording"
)

// defaultTranscriptLineCap bounds the transcript edges fetched in one read so a
// pathologically long call can't blow up the recap prompt or memory. The caller
// may pass a smaller cap.
const defaultTranscriptLineCap = 5000

// buildTranscriptsByRoomQuery builds the Dgraph read that resolves a room to its
// newest non-deleted recording (channel OR DM/group) and pulls that recording's
// transcript edges in chronological order plus its egress id. The query lives in
// the domain layer (queries are written here; the model only executes) and takes
// $room as a bound variable; limit is composed into the selection. Pure.
//
// Room → recording resolution mirrors the call-room naming (see
// helpers.GetGroupingId): channel call → room == ch_uuid; 1:1 DM / group chat →
// room == dm_grouping_id. Both resolutions run in one query; the model takes
// whichever surface owns a recording. recording_deleted_at is excluded, but
// recording_ended_at is NOT required (room_finished may fire before it is set).
func buildTranscriptsByRoomQuery(limit int) string {
	transcriptSel := fmt.Sprintf(`
		recording_transcript (orderasc: transcript_timestamp, first: %d) {
			transcript_text
			transcript_timestamp
			transcript_offset_ms
			transcript_from { uid }
		}`, limit)

	const recFilter = `not gt(recording_deleted_at, "1970-01-01T00:00:00Z")`

	return fmt.Sprintf(`query Transcripts($room: string){
		channelRec(func: eq(ch_uuid, $room)) {
			ch_recording @filter(%s) (orderdesc: recording_stared_at, first: 1) {
				recording_egress_id
				%s
			}
		}
		dmRec(func: eq(dm_grouping_id, $room)) {
			dm_recording @filter(%s) (orderdesc: recording_stared_at, first: 1) {
				recording_egress_id
				%s
			}
		}
	}`, recFilter, transcriptSel, recFilter, transcriptSel)
}

// TranscriptLine is one ordered utterance in a room, store-agnostic, consumed
// by the meeting-recap agent to reconstruct the conversation.
type TranscriptLine struct {
	// ParticipantIdentity is the speaker's Dgraph node uid (the identity the
	// LiveKit token carries), resolved to a display name by the recap agent.
	ParticipantIdentity string
	Text                string
	// Timestamp is the absolute utterance time in ms (producer clock); used
	// only for chronological ordering.
	Timestamp int64
	// OffsetMs is the skew-free utterance start in ms from recording start
	// (producer clock). 0 when the producing client didn't supply it.
	OffsetMs int64
}

// GetTranscriptLinesByRoom returns the chronological transcript for a room's
// most-recent recording, plus that recording's egress id (for deep-linking to
// the recording). Returns an empty slice (not an error) when the room has no
// recording or transcript, so the caller treats it as a benign skip.
func GetTranscriptLinesByRoom(ctx context.Context, roomName string, limit int) ([]TranscriptLine, string, error) {
	if roomName == "" {
		return nil, "", nil
	}
	if limit <= 0 || limit > defaultTranscriptLineCap {
		limit = defaultTranscriptLineCap
	}
	query := buildTranscriptsByRoomQuery(limit)
	variables := map[string]string{"$room": roomName}

	nodes, egressID, err := recordingDgraph.GetDgraphTranscriptsByRoom(ctx, query, variables)
	if err != nil {
		return nil, "", err
	}
	// A call session key is not an egress id and there is no media behind it.
	// Returned as empty so every caller behaves as it always did when there was
	// no recording: the recap in particular uses this to decide whether to offer
	// a "play recording" button, and a button that opens nothing is worse than
	// no button. Dropped HERE, at the one place that resolves a room to its
	// transcript, so no caller has to know the key can be either kind.
	if helpers.IsCallSessionKey(egressID) {
		egressID = ""
	}
	out := make([]TranscriptLine, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		line := TranscriptLine{Text: n.Text}
		if n.From != nil {
			line.ParticipantIdentity = n.From.Uid
		}
		if n.TimeStamp != nil {
			line.Timestamp = *n.TimeStamp
		}
		if n.OffsetMs != nil {
			line.OffsetMs = *n.OffsetMs
		}
		out = append(out, line)
	}
	return out, egressID, nil
}
