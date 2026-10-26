//go:build integration
// +build integration

package integration_test

// A cascade right after a write marks what was just written. An
// update-by-query matches only what the last refresh made visible, and a write
// (a bulk one, an import's) doesn't refresh, so a task trashed within a second
// of its comments' writing left them findable: the reindex's repair test lost
// its trashed comment that way in 9 runs of 13. The comments index refreshes
// on its own every second; here it doesn't, so the race can't hide. Against a
// real OpenSearch with the index's own mapping.
//
// Run: go test -tags=integration ./tests/integration/ -run TestACascadeRightAfterAWriteMarksWhatWasJustWritten -v

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	globalSearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestACascadeRightAfterAWriteMarksWhatWasJustWritten(t *testing.T) {
	ctx := context.Background()
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	client := opensearchInit.OpenSearchClient
	refreshEvery := func(interval string) {
		t.Helper()
		if _, err := client.Indices.Settings.Put(ctx, opensearchapi.SettingsPutReq{Indices: []string{openSearchStruct.COMMENT_INDEX},
			Body: strings.NewReader(`{"index": {"refresh_interval": ` + interval + `}}`)}); err != nil {
			t.Fatal(err)
		}
	}
	refreshEvery(`"-1"`)
	t.Cleanup(func() { refreshEvery("null") })

	task, comment, other := uuid.NewString(), uuid.NewString(), uuid.NewString()
	write := func(id, taskID string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"comment_id": id, "comment_task_id": taskID, "comment_body": "Kiwi idea notes"})
		if _, err := client.Index(ctx, opensearchapi.IndexReq{Index: openSearchStruct.COMMENT_INDEX, DocumentID: id, Body: strings.NewReader(string(raw))}); err != nil {
			t.Fatal(err)
		}
	}
	write(comment, task)
	write(other, uuid.NewString())

	if err := globalSearchModels.SyncCascadingDeletionInOpenSearch(ctx, []string{"comment_task_id"}, task, time.Now().Unix(),
		[]string{openSearchStruct.COMMENT_INDEX}, "cascade"); err != nil {
		t.Fatal(err)
	}

	// A get by id reads the entry as last written, refreshed or not.
	deleted := func(id string) bool {
		t.Helper()
		resp, err := client.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: openSearchStruct.COMMENT_INDEX, DocumentID: id})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Inspect().Response.Body)
		var got struct {
			Source map[string]any `json:"_source"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		_, ok := got.Source["deleted_date"]
		return ok
	}
	if !deleted(comment) {
		t.Error("a comment written just before its task went to the trash stays findable")
	}
	if deleted(other) {
		t.Error("a comment of another task was marked deleted")
	}
}
