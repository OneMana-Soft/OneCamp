package domain

import (
	"strings"
	"testing"
	"time"
)

// CALL-BUG-3. A call's transcript gets an end when the call ends: when it
// began, when it ended, and how long it ran. Everything that handles a
// recording takes only ended ones, so a transcript without an end was
// invisible, undeletable and kept for ever.
func TestSessionEndFields(t *testing.T) {
	began := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	ended := began.Add(25 * time.Minute)
	f := sessionEndFields(began, ended)
	if f["recording_ended_at"] != ended || f["recording_stared_at"] != began || f["recording_duration"] != 1500.0 {
		t.Errorf("fields = %v", f)
	}
	// An unknown start keeps the node's own.
	if f := sessionEndFields(time.Time{}, ended); len(f) != 1 || f["recording_ended_at"] != ended {
		t.Errorf("fields without a start = %v", f)
	}
}

func TestParseCallSessionNodes(t *testing.T) {
	raw := []byte(`{"recordingInfo":[
		{"uid":"0x1","recording_egress_id":"call-RM_a","recording_stared_at":"2026-10-11T10:00:00Z","linked_channel":1,"linked_dm":0,
		 "recording_transcript":[{"transcript_timestamp":1791714600000}]},
		{"uid":"0x2","recording_egress_id":"call-RM_b","linked_channel":0,"linked_dm":0},
		{"uid":"0x3","recording_egress_id":"call-RM_c","recording_ended_at":"2026-10-11T11:00:00Z","linked_channel":0,"linked_dm":1}
	]}`)
	nodes, err := parseCallSessionNodes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 || !nodes[0].Linked || nodes[1].Linked || !nodes[2].Linked {
		t.Fatalf("nodes = %+v", nodes)
	}
	if nodes[0].LastLine.UnixMilli() != 1791714600000 || nodes[0].LastActivity() != nodes[0].LastLine {
		t.Errorf("last line = %v", nodes[0].LastLine)
	}
	if !nodes[1].LastActivity().IsZero() || nodes[2].EndedAt.IsZero() {
		t.Errorf("times = %+v", nodes)
	}
}

// The repair reads nodes with no media file: real recordings have one from the
// moment they start, so they are never touched.
func TestTheRepairNeverReadsARealRecording(t *testing.T) {
	src := callSessionNodesQueryForTest()
	if !strings.Contains(src, "not has(recording_obj_key)") || !strings.Contains(src, "count(recording_channel)") || !strings.Contains(src, "count(recording_dm)") {
		t.Errorf("query = %s", src)
	}
}

func callSessionNodesQueryForTest() string { return callSessionNodesQuery }
