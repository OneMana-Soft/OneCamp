package domain

// Transcript domain — Dgraph-backed.
//
// Transcripts live exclusively in Dgraph as recording_transcript edges on the
// recording node (there is no Postgres transcript table). This domain resolves
// a room to its newest recording and returns the transcript as ordered,
// store-agnostic lines for the meeting-recap agent.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
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

// buildCallTranscriptQuery reads ONE call's transcript: the lines on its
// session node, and the lines on recordings made during it in the same
// conversation (lines go to the recording while one runs, so the player's
// transcript lines up). Pure.
//
// The recap used to read "the newest recording of the conversation", which
// nothing tied to the call that had just ended: a call with no transcript
// summarised the previous call again.
func buildCallTranscriptQuery(limit int) string {
	transcriptSel := fmt.Sprintf(`
			recording_transcript (orderasc: transcript_timestamp, first: %d) {
				transcript_text
				transcript_timestamp
				transcript_offset_ms
				transcript_from { uid }
			}`, limit)

	const live = `not gt(recording_deleted_at, "1970-01-01T00:00:00Z")`
	const during = live + ` AND NOT eq(recording_transcript_only, true) AND ge(recording_stared_at, $from) AND le(recording_stared_at, $to)`

	return fmt.Sprintf(`query CallTranscript($key: string, $room: string, $from: string, $to: string){
		session(func: eq(recording_egress_id, $key)) @filter(%s) {
			recording_egress_id
			%s
		}
		channelRec(func: eq(ch_uuid, $room)) {
			ch_recording @filter(%s) {
				recording_egress_id
				recording_stared_at
				%s
			}
		}
		dmRec(func: eq(dm_grouping_id, $room)) {
			dm_recording @filter(%s) {
				recording_egress_id
				recording_stared_at
				%s
			}
		}
	}`, live, transcriptSel, during, transcriptSel, during, transcriptSel)
}

// GetCallTranscriptLines returns one call's transcript in order, and the
// egress of the last recording made during it ("" for none), for the recap's
// "play recording". The call is its session key and room, and the window it
// ran in. Empty, not an error, for a call with no transcript.
func GetCallTranscriptLines(ctx context.Context, roomName, sessionKey string, from, to time.Time, limit int) ([]TranscriptLine, string, error) {
	if roomName == "" || !helpers.IsCallSessionKey(sessionKey) {
		return nil, "", nil
	}
	if limit <= 0 || limit > defaultTranscriptLineCap {
		limit = defaultTranscriptLineCap
	}
	raw, err := recordingDgraph.QueryRecordingsJSON(ctx, buildCallTranscriptQuery(limit), map[string]string{
		"$key":  sessionKey,
		"$room": roomName,
		"$from": from.UTC().Format(time.RFC3339),
		"$to":   to.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, "", err
	}
	return parseCallTranscript(raw, limit)
}

// parseCallTranscript merges the session's lines and its recordings' into one
// timeline. Pure.
func parseCallTranscript(raw []byte, limit int) ([]TranscriptLine, string, error) {
	var parsed struct {
		Session    []*dgraphStruct.DgraphRecording `json:"session"`
		ChannelRec []struct {
			Recording []*dgraphStruct.DgraphRecording `json:"ch_recording"`
		} `json:"channelRec"`
		DMRec []struct {
			Recording []*dgraphStruct.DgraphRecording `json:"dm_recording"`
		} `json:"dmRec"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, "", err
	}
	recs := append([]*dgraphStruct.DgraphRecording{}, parsed.Session...)
	var egress string
	var egressAt time.Time
	during := func(list []*dgraphStruct.DgraphRecording) {
		for _, r := range list {
			if r == nil {
				continue
			}
			recs = append(recs, r)
			if r.StartedAt != nil && !r.StartedAt.Before(egressAt) {
				egress, egressAt = r.EgressId, *r.StartedAt
			}
		}
	}
	for _, c := range parsed.ChannelRec {
		during(c.Recording)
	}
	for _, d := range parsed.DMRec {
		during(d.Recording)
	}

	var out []TranscriptLine
	for _, r := range recs {
		for _, n := range r.Transcript {
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
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, egress, nil
}
