package domain

// A call's session node holds the transcript of a call that has no recording,
// and is linked to the conversation the call was in. These are its reads and
// writes beyond the ones every recording has.

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Recording"
)

// EndCallSessionNode stamps a call's session node as over: when the call began
// (when known) and ended, and so how long it ran.
//
// A session node was never given an end, and an end is what everything that
// handles a recording asks for: the conversation's list, the delete endpoints,
// and the retention policy all take only ended recordings, so a transcript of
// an unrecorded call was invisible, undeletable and kept for ever. Never makes
// a node (nothing is written when there is none for the key), and an end
// already stamped is left alone, so a webhook delivered twice changes nothing.
func EndCallSessionNode(ctx context.Context, key string, began, ended time.Time) error {
	query := `query Session($key: string) {
		session as var(func: eq(recording_egress_id, $key)) @filter(eq(recording_transcript_only, true) AND NOT gt(recording_ended_at, "1970-01-01T00:00:00Z"))
	}`
	return dgraphModels.SetFoundRecordingsFields(ctx, query, map[string]string{"$key": key}, "session", sessionEndFields(began, ended))
}

// sessionEndFields is what ending a session node writes. Pure.
func sessionEndFields(began, ended time.Time) map[string]interface{} {
	fields := map[string]interface{}{"recording_ended_at": ended}
	if !began.IsZero() && began.Before(ended) {
		fields["recording_stared_at"] = began
		fields["recording_duration"] = ended.Sub(began).Seconds()
	}
	return fields
}

// CallSessionNode is what the boot repair needs to know about one node with a
// call's words and no media.
type CallSessionNode struct {
	Uid       string
	Key       string
	StartedAt time.Time
	EndedAt   time.Time
	DeletedAt time.Time
	// Linked is whether it belongs to a channel or a DM, which is how anybody
	// can ever find it.
	Linked bool
	// LastLine is when its newest line was spoken; zero when it has none.
	LastLine time.Time
}

// LastActivity is the newest thing known about the node: its last line, or
// when it started.
func (n CallSessionNode) LastActivity() time.Time {
	if n.LastLine.After(n.StartedAt) {
		return n.LastLine
	}
	return n.StartedAt
}

// CallSessionNodes lists, one page at a time in uid order after the given uid
// ("0x0" for the first page), every recording node with no media file. Real
// recordings always have a file key, from the moment they start; the ones
// without are call session nodes, and the strays transcript lines made for
// rooms that belonged to no conversation.
func CallSessionNodes(ctx context.Context, after string, first int) ([]CallSessionNode, error) {
	raw, err := dgraphModels.QueryRecordingsJSON(ctx, callSessionNodesQuery, map[string]string{
		"$first": strconv.Itoa(first),
		"$after": after,
	})
	if err != nil {
		return nil, err
	}
	return parseCallSessionNodes(raw)
}

// callSessionNodesQuery is CallSessionNodes' read.
const callSessionNodesQuery = `query Sessions($first: int, $after: string) {
		recordingInfo(func: has(recording_egress_id), first: $first, after: $after) @filter(not has(recording_obj_key)) {
			uid
			recording_egress_id
			recording_stared_at
			recording_ended_at
			recording_deleted_at
			linked_channel: count(recording_channel)
			linked_dm: count(recording_dm)
			recording_transcript (orderdesc: transcript_timestamp, first: 1) {
				transcript_timestamp
			}
		}
	}`

// parseCallSessionNodes reads CallSessionNodes' answer. Pure.
func parseCallSessionNodes(raw []byte) ([]CallSessionNode, error) {
	var resp struct {
		RecordingInfo []struct {
			Uid           string     `json:"uid"`
			Key           string     `json:"recording_egress_id"`
			StartedAt     *time.Time `json:"recording_stared_at"`
			EndedAt       *time.Time `json:"recording_ended_at"`
			DeletedAt     *time.Time `json:"recording_deleted_at"`
			LinkedChannel int        `json:"linked_channel"`
			LinkedDm      int        `json:"linked_dm"`
			Transcript    []struct {
				At int64 `json:"transcript_timestamp"`
			} `json:"recording_transcript"`
		} `json:"recordingInfo"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	out := make([]CallSessionNode, 0, len(resp.RecordingInfo))
	for _, r := range resp.RecordingInfo {
		n := CallSessionNode{Uid: r.Uid, Key: r.Key, Linked: r.LinkedChannel > 0 || r.LinkedDm > 0}
		if r.StartedAt != nil {
			n.StartedAt = *r.StartedAt
		}
		if r.EndedAt != nil {
			n.EndedAt = *r.EndedAt
		}
		if r.DeletedAt != nil {
			n.DeletedAt = *r.DeletedAt
		}
		if len(r.Transcript) > 0 && r.Transcript[0].At > 0 {
			n.LastLine = time.UnixMilli(r.Transcript[0].At)
		}
		out = append(out, n)
	}
	return out, nil
}

// MarkCallSessionNodeEnded stamps the end on one session node found by the
// repair, flagging it transcript-only on the way (the strays were never
// flagged). Never makes a node.
func MarkCallSessionNodeEnded(ctx context.Context, key string, ended time.Time) error {
	query := `query Session($key: string) {
		session as var(func: eq(recording_egress_id, $key)) @filter(not has(recording_obj_key) AND NOT gt(recording_ended_at, "1970-01-01T00:00:00Z"))
	}`
	err := dgraphModels.SetFoundRecordingsFields(ctx, query, map[string]string{"$key": key}, "session", map[string]interface{}{
		"recording_ended_at":        ended,
		"recording_transcript_only": true,
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MarkCallSessionNodeEnded failed: %v", err)
	}
	return err
}

// FindCallSessionNode answers the uid of the node holding a call's
// transcript, and whether it is linked to a conversation. "" when there is
// none yet.
func FindCallSessionNode(ctx context.Context, key string) (uid string, linked bool, err error) {
	query := `query Session($key: string) {
		recordingInfo(func: eq(recording_egress_id, $key)) {
			uid
			linked_channel: count(recording_channel)
			linked_dm: count(recording_dm)
		}
	}`
	raw, err := dgraphModels.QueryRecordingsJSON(ctx, query, map[string]string{"$key": key})
	if err != nil {
		return "", false, err
	}
	var resp struct {
		RecordingInfo []struct {
			Uid           string `json:"uid"`
			LinkedChannel int    `json:"linked_channel"`
			LinkedDm      int    `json:"linked_dm"`
		} `json:"recordingInfo"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", false, err
	}
	for _, r := range resp.RecordingInfo {
		if r.LinkedChannel > 0 || r.LinkedDm > 0 {
			return r.Uid, true, nil
		}
		uid = r.Uid
	}
	return uid, false, nil
}

// AttachTranscriptLine adds a line to the recording or call session with this
// key, if and only if it exists, and answers whether it did. It never makes a
// node: a line for a key with none used to make one, holding only the key and
// the line, linked to no conversation, which nobody could see or delete and
// retention never reached.
func AttachTranscriptLine(ctx context.Context, key string, line *dgraphStruct.DgraphTranscript) (bool, error) {
	query := `query Recording($key: string) {
		recording as var(func: eq(recording_egress_id, $key)) @filter(has(recording_stared_at))
	}`
	return dgraphModels.AddTranscriptToFound(ctx, query, map[string]string{"$key": key}, "recording", line)
}
