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

// CALL-BUG-6. The recap reads ONE call: the lines on its session node, and on
// recordings made during it in the same conversation. It used to read "the
// newest recording of the conversation", which nothing tied to the call that
// had just ended, so a call with no transcript re-posted the previous call's
// recap.
func TestBuildCallTranscriptQueryReadsOnlyThatCall(t *testing.T) {
	q := buildCallTranscriptQuery(4000)
	for _, want := range []string{
		`query CallTranscript($key: string, $room: string, $from: string, $to: string)`,
		`session(func: eq(recording_egress_id, $key))`,
		`channelRec(func: eq(ch_uuid, $room))`,
		`dmRec(func: eq(dm_grouping_id, $room))`,
		`ge(recording_stared_at, $from) AND le(recording_stared_at, $to)`,
		`NOT eq(recording_transcript_only, true)`,
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("call transcript query missing %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, "orderdesc: recording_stared_at, first: 1") {
		t.Fatalf("the query still picks the conversation's newest recording:\n%s", q)
	}
}

func TestParseCallTranscriptMergesTheCallsLinesInOrder(t *testing.T) {
	raw := []byte(`{
		"session":[{"recording_egress_id":"call-RM_1","recording_transcript":[
			{"transcript_text":"before the recording","transcript_timestamp":100,"transcript_from":{"uid":"0x1"}},
			{"transcript_text":"after the recording","transcript_timestamp":400,"transcript_from":{"uid":"0x2"}}]}],
		"channelRec":[{"ch_recording":[
			{"recording_egress_id":"EG_1","recording_stared_at":"2026-10-13T10:01:00Z","recording_transcript":[
				{"transcript_text":"while recording","transcript_timestamp":200,"transcript_offset_ms":5,"transcript_from":{"uid":"0x1"}}]},
			{"recording_egress_id":"EG_2","recording_stared_at":"2026-10-13T10:05:00Z","recording_transcript":[
				{"transcript_text":"second recording","transcript_timestamp":300,"transcript_from":{"uid":"0x2"}}]}]}],
		"dmRec":[]
	}`)
	lines, egress, err := parseCallTranscript(raw, 10)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, l := range lines {
		texts = append(texts, l.Text)
	}
	if got := strings.Join(texts, "|"); got != "before the recording|while recording|second recording|after the recording" {
		t.Errorf("lines = %s", got)
	}
	if egress != "EG_2" {
		t.Errorf("the recap links the last recording made during the call, got %q", egress)
	}
	if lines[1].OffsetMs != 5 || lines[0].ParticipantIdentity != "0x1" {
		t.Errorf("lines = %+v", lines)
	}
	if capped, _, _ := parseCallTranscript(raw, 2); len(capped) != 2 {
		t.Errorf("the cap was not applied: %d lines", len(capped))
	}
	if none, egress, err := parseCallTranscript([]byte(`{}`), 10); err != nil || len(none) != 0 || egress != "" {
		t.Errorf("a call with no transcript: %v %q %v", none, egress, err)
	}
}
