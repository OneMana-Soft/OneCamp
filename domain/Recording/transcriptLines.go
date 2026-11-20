package domain

// The graph reads a line's search entry is built from
// (business/TranscriptSearch): the home a line is filed under (a recording's
// egress id, or a call session's key), as the graph has it now, with its
// lines. The words are read here and nowhere else on the way to the index.

import (
	"context"
	"errors"
	"strconv"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Recording"
)

// transcriptHomeFields are what an entry takes from its home: the call's
// start, whether it has media to play, whether it's deleted, and the
// conversation it was in.
const transcriptHomeFields = `
			uid
			recording_egress_id
			recording_stared_at
			recording_obj_key
			recording_deleted_at
			recording_channel { ch_uuid }
			recording_dm { dm_grouping_id }`

// transcriptLineFields are what an entry takes from a line.
const transcriptLineFields = `
				uid
				transcript_text
				transcript_timestamp
				transcript_offset_ms
				transcript_from { user_uuid user_full_name }`

// TranscriptLineHome is the started home filed under key with only the line
// lineUID under it (none when the line isn't under it); nil when no started
// home has that key. The ids are query variables, never query text.
func TranscriptLineHome(ctx context.Context, key, lineUID string) (*dgraphStruct.DgraphRecording, error) {
	if key == "" || lineUID == "" {
		return nil, nil
	}
	query := `query Line($key: string, $line: string) {
		recordingInfo(func: eq(recording_egress_id, $key)) @filter(has(recording_stared_at)) {` + transcriptHomeFields + `
			recording_transcript @filter(uid($line)) {` + transcriptLineFields + `
			}
		}
	}`
	return oneTranscriptHome(ctx, query, map[string]string{"$key": key, "$line": lineUID})
}

// TranscriptHomeLines is the started home filed under key with every line;
// nil when no started home has that key.
func TranscriptHomeLines(ctx context.Context, key string) (*dgraphStruct.DgraphRecording, error) {
	if key == "" {
		return nil, nil
	}
	query := `query Lines($key: string) {
		recordingInfo(func: eq(recording_egress_id, $key)) @filter(has(recording_stared_at)) {` + transcriptHomeFields + `
			recording_transcript {` + transcriptLineFields + `
			}
		}
	}`
	return oneTranscriptHome(ctx, query, map[string]string{"$key": key})
}

// TranscribedHomes is a page of live started homes that hold lines and are
// linked to a channel or a chat, after the uid after ("0x0" for the first
// page), each with every line: the backfill's read.
func TranscribedHomes(ctx context.Context, after string, first int) ([]*dgraphStruct.DgraphRecording, error) {
	query := `query Homes($first: int, $after: string) {
		recordingInfo(func: has(recording_transcript), first: $first, after: $after) @filter(has(recording_stared_at) AND ` +
		dgraphquery.Live("recording_deleted_at") + ` AND (has(recording_channel) OR has(recording_dm))) {` + transcriptHomeFields + `
			recording_transcript {` + transcriptLineFields + `
			}
		}
	}`
	return dgraphModels.GetDgraphRecordingsList(ctx, query, map[string]string{"$first": strconv.Itoa(first), "$after": after})
}

// oneTranscriptHome is the one home a read by key answers; nil for none.
func oneTranscriptHome(ctx context.Context, query string, variables map[string]string) (*dgraphStruct.DgraphRecording, error) {
	home, err := dgraphModels.GetDgraphRecordingInfo(ctx, query, variables)
	if errors.Is(err, dgraphModels.ErrNotFound) {
		return nil, nil
	}
	return home, err
}

// LineOffsetMs is the moment a line was said in its call, in milliseconds:
// its own offset, or for a line filed before lines carried one, its time less
// the call's start (startedMs), as the player reckons it. Search entries and
// exported files both take it from here.
func LineOffsetMs(l *dgraphStruct.DgraphTranscript, startedMs int64) int64 {
	if l.OffsetMs != nil {
		return *l.OffsetMs
	}
	if l.TimeStamp != nil && startedMs > 0 && *l.TimeStamp > startedMs {
		return *l.TimeStamp - startedMs
	}
	return 0
}
