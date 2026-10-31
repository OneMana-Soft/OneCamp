package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
)

// A parent going away (a doc archived or deleted, a channel or project
// archived) marks its comments', files' and AI entries deleted, "cascade",
// and bringing the parent back clears what is marked so. Each cascade marked
// every entry it matched: a comment its author had deleted was marked
// "cascade" too, and came back to search when its doc was restored. All three
// cascades now send a script that reads the entry's deletion and stops (noop)
// on one deleted already, before it writes anything. What OpenSearch does with
// each kind of entry is tested against a real one in tests/integration
// (TestACascadeLeavesWhatWasDeletedBeforeItAsItWas).
func TestEveryCascadeLeavesAnEntryDeletedAlreadyAsItIs(t *testing.T) {
	type request struct {
		path   string
		source string
		params map[string]any
	}
	var mu sync.Mutex
	var got []request
	var paths []string
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/_refresh") {
			fmt.Fprint(w, `{"_shards":{"total":1,"successful":1,"failed":0}}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Script struct {
				Source string         `json:"source"`
				Params map[string]any `json:"params"`
			} `json:"script"`
			Query map[string]any `json:"query"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || len(body.Query) == 0 {
			t.Errorf("%s: a body that isn't a script and a query (%v):\n%s", r.URL.Path, err, raw)
		}
		mu.Lock()
		got = append(got, request{r.URL.Path, body.Script.Source, body.Script.Params})
		mu.Unlock()
		fmt.Fprint(w, `{"took":1,"total":0,"updated":0,"noops":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	defer func() { opensearchInit.OpenSearchClient = nil }()

	ctx := context.Background()
	const at = 1760000000
	if err := SyncCascadingDeletionInOpenSearch(ctx, []string{"comment_doc_id"}, "doc-1", at, []string{"comments"}, "cascade"); err != nil {
		t.Fatal(err)
	}
	_ = SyncCascadingDeletionInOpenSearchMulti(ctx, "attachment_id", []string{"file-1"}, at, []string{"attachments"}, "cascade")
	_ = SyncCascadingDeletionInOpenSearchCombined(ctx, map[string][]string{"doc_uuid": {"doc-1"}}, at, []string{"ai_embeddings"}, "cascade")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("%d update-by-queries for three cascades: %+v", len(got), got)
	}
	// Each refreshes its index first: an update-by-query matches only what the
	// last refresh made visible.
	want := []string{"/comments/_refresh", "/comments/_update_by_query", "/attachments/_refresh", "/attachments/_update_by_query",
		"/ai_embeddings/_refresh", "/ai_embeddings/_update_by_query"}
	if !slices.Equal(paths, want) {
		t.Errorf("requests %v; want each cascade's index refreshed right before its update-by-query", paths)
	}
	for i, index := range []string{"comments", "attachments", "ai_embeddings"} {
		r := got[i]
		if r.path != "/"+index+"/_update_by_query" {
			t.Errorf("cascade %d went to %s, want %s's update by query", i, r.path, index)
		}
		stop := strings.Index(r.source, "ctx.op = 'noop'")
		write := strings.Index(r.source, "ctx._source.deleted_date = params.deleted_at")
		if stop < 0 || write < 0 || stop > write || !strings.Contains(r.source[:stop], "ctx._source.deleted_date") {
			t.Errorf("the %s cascade writes over an entry deleted already:\n%s", index, r.source)
		}
		if r.params["deleted_at"] != float64(at) || r.params["deleted_by"] != "cascade" {
			t.Errorf("the %s cascade marks entries with %v", index, r.params)
		}
	}
}

// A settle bounds how long a change waits for search to stop granting what it
// took (business/Board settleBoardDelete, business/Doc searchFollowsTrash).
// The cascades ran their update under context.Background(), so a run still
// held by another write waited out every retry past that bound. Each now
// stops when its caller does.
func TestACascadeStopsWhenItsCallerDoes(t *testing.T) {
	fakeUpdates(t, opensearchapi.UpdateByQueryResp{VersionConflicts: 1})
	conflictBackoff = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	byDoc := map[string][]string{"comment_doc_id": {"d1"}}
	comments := []string{"comments"}
	cascades := map[string]func() error{
		"deletion": func() error {
			return SyncCascadingDeletionInOpenSearch(ctx, []string{"comment_doc_id"}, "d1", 1, comments, "cascade")
		},
		"deletion of many": func() error {
			return SyncCascadingDeletionInOpenSearchMulti(ctx, "comment_doc_id", []string{"d1"}, 1, comments, "cascade")
		},
		"combined deletion": func() error {
			return SyncCascadingDeletionInOpenSearchCombined(ctx, byDoc, 1, comments, "cascade")
		},
		"unarchive": func() error {
			return SyncCascadingUnarchiveInOpenSearch(ctx, []string{"comment_doc_id"}, "d1", comments, "cascade")
		},
		"unarchive of many": func() error {
			return SyncCascadingUnarchiveInOpenSearchMulti(ctx, "comment_doc_id", []string{"d1"}, comments, "cascade")
		},
		"combined unarchive": func() error {
			return SyncCascadingUnarchiveInOpenSearchCombined(ctx, byDoc, comments, "cascade")
		},
	}
	for name, run := range cascades {
		done := make(chan error, 1)
		go func() { done <- run() }()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("the %s cascade answered %v once its caller had stopped; want context.Canceled", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("the %s cascade kept waiting to run again after its caller had stopped", name)
		}
	}
}
