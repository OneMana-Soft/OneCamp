package domain

import (
	"strings"
	"testing"
)

func TestBuildTranscriptsByRoomQuery(t *testing.T) {
	q := buildTranscriptsByRoomQuery(4000)
	for _, want := range []string{
		`query Transcripts($room: string)`,
		`channelRec(func: eq(ch_uuid, $room))`,
		`dmRec(func: eq(dm_grouping_id, $room))`,
		`recording_transcript (orderasc: transcript_timestamp, first: 4000)`,
		`recording_egress_id`,
		`not gt(recording_deleted_at, "1970-01-01T00:00:00Z")`,
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("transcript query missing %q:\n%s", want, q)
		}
	}
	// Both surfaces reuse the same transcript selection (channel + DM).
	if strings.Count(q, "recording_transcript (orderasc:") != 2 {
		t.Fatalf("expected the transcript selection in both surface blocks:\n%s", q)
	}
}
