package domain

import (
	"context"
	"strings"
	"testing"
)

// MB3: two nodes under one key (a session's own node and its recording's) are
// one home, deleted if either node is, whichever the graph answers first.
func TestATranscriptHomeIsDeletedIfEitherOfItsNodesIs(t *testing.T) {
	node := func(deletedAt string) string {
		return `{"recording_egress_id":"EG_1","recording_deleted_at":"` + deletedAt +
			`","recording_ended_at":"2026-11-01T09:00:00Z","recording_channel":{"ch_uuid":"ch-1"}}`
	}
	live, deleted := node("1970-01-01T00:00:00Z"), node("2026-11-01T10:00:00Z")
	was := homesQuery
	t.Cleanup(func() { homesQuery = was })
	for _, nodes := range [][]string{{deleted, live}, {live, deleted}, {live, live}} {
		homesQuery = func(context.Context, string, map[string]string) ([]byte, error) {
			return []byte(`{"recordingInfo":[` + strings.Join(nodes, ",") + `]}`), nil
		}
		homes, err := TranscriptHomes(context.Background(), []string{"EG_1"}, nil)
		want := nodes[0] == deleted || nodes[1] == deleted
		if home, ok := homes["EG_1"]; err != nil || !ok || home.Deleted != want || !home.Ended || home.ConvID != "ch-1" {
			t.Errorf("two nodes (first deleted %v, second deleted %v) read as %+v (%v); want deleted %v, ended, in ch-1",
				nodes[0] == deleted, nodes[1] == deleted, home, err, want)
		}
	}
}

// CR-425 F2: only the export's read takes a line's kept translations; the
// correction's read never pulls them, so its answer can't carry them.
func TestOnlyTheExportReadTakesALinesKeptTranslations(t *testing.T) {
	if !strings.Contains(readerQuery(), "transcript_translations") {
		t.Fatal("the export's read must take the kept translations (a ?lang= read answers from them)")
	}
	if strings.Contains(correctionQuery(), "transcript_translations") {
		t.Fatal("the correction's read takes the kept translations; it must never read them")
	}
}
