package business

import (
	"context"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// zeroTime is how the graph reads a live node's deletion time: 0001-01-01,
// not "no value". A bare DeletedAt != nil reads it as deleted.
var zeroTime = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)

// The restore list holds only what was deleted: a live doc or recording the
// graph answered with a 0001-01-01 deletion time is left out, not listed as
// archived at year one.
func TestTheRestoreListLeavesOutLiveRows(t *testing.T) {
	prevDocs, prevRead, prevRecs := recentlyDeletedDocs, docsCouldRead, recentlyArchivedRecordings
	t.Cleanup(func() { recentlyDeletedDocs, docsCouldRead, recentlyArchivedRecordings = prevDocs, prevRead, prevRecs })
	gone := time.Now().Add(-time.Hour)
	live := zeroTime
	recentlyDeletedDocs = func(context.Context, string, int, int) ([]*dgraphStruct.DgraphDoc, int64, error) {
		return []*dgraphStruct.DgraphDoc{
			{Uuid: "0d0c0000-0000-4000-8000-000000000001", Title: "gone", DeletedAt: &gone},
			{Uuid: "0d0c0000-0000-4000-8000-000000000002", Title: "live", DeletedAt: &live},
		}, 2, nil
	}
	docsCouldRead = func(context.Context, []string, string) (map[string]bool, error) { return map[string]bool{}, nil }
	recentlyArchivedRecordings = func(context.Context, string, int, int) ([]*dgraphStruct.DgraphRecording, int64, error) {
		return []*dgraphStruct.DgraphRecording{
			{EgressId: "EG_gone_123", DeletedAt: &gone},
			{EgressId: "EG_live_456", DeletedAt: &live},
		}, 2, nil
	}
	docs, _, err := getRecentlyArchivedDocs(context.Background(), 10, 0, "Document")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Id != "0d0c0000-0000-4000-8000-000000000001" {
		t.Errorf("docs listed: %+v; want only the deleted one", docs)
	}
	recs, _, err := getRecentlyArchivedRecordings(context.Background(), 10, 0, "Recording")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Id != "EG_gone_123" {
		t.Errorf("recordings listed: %+v; want only the deleted one", recs)
	}
}
