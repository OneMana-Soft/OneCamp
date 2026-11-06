//go:build integration
// +build integration

package integration_test

// A purge's search delete (models/openSearch/GlobalSearch ForgetWhere, the
// search sink of every purge past the trash) over the real OpenSearch: it
// takes entries no refresh has shown yet, and skips an index that doesn't
// exist (the AI index where AI is off) without an error. A delete-by-query
// sees only what the last refresh showed, and an index nothing searched for
// 30 s goes search-idle and skips its scheduled refreshes: b204's purged
// board kept its comment's entry, findable after the purge.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAPurgeTakesEntriesNoRefreshHasShownYet -v

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	globalSearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAPurgeTakesEntriesNoRefreshHasShownYet(t *testing.T) {
	ctx := context.Background()
	integration.SetupOpenSearch(t)
	search := opensearchInit.OpenSearchClient

	// An index that never refreshes by itself: what is written to it stays unseen by a search until a refresh,
	// always, as a search-idle index's writes do until something searches it.
	index := "forget-unrefreshed-" + uuid.NewString()[:8]
	if _, err := search.Indices.Create(ctx, opensearchapi.IndicesCreateReq{Index: index,
		Body: strings.NewReader(`{"settings":{"index":{"refresh_interval":"-1","number_of_replicas":0}}}`)}); err != nil {
		t.Fatalf("the index: %v", err)
	}
	purged, kept := uuid.NewString(), uuid.NewString()
	for _, board := range []string{purged, purged, kept} {
		body := `{"comment_board_id":"` + board + `","comment_text":"a note"}`
		if _, err := search.Index(ctx, opensearchapi.IndexReq{Index: index, DocumentID: uuid.NewString(), Body: strings.NewReader(body)}); err != nil {
			t.Fatalf("a comment's entry: %v", err)
		}
	}

	missing := "forget-missing-" + uuid.NewString()[:8]
	n, err := globalSearchModels.ForgetWhere(ctx, []string{index, missing}, []string{"comment_board_id"}, []string{purged})
	if err != nil {
		t.Fatalf("ForgetWhere: %v; want an index that doesn't exist skipped, not an error", err)
	}
	if n != 2 {
		t.Errorf("ForgetWhere removed %d entries; want the purged board's 2, written but not yet refreshed", n)
	}

	entries := func(board string) int {
		t.Helper()
		if _, err := search.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{index}}); err != nil {
			t.Fatal(err)
		}
		q := `{"query":{"term":{"comment_board_id.keyword":"` + board + `"}}}`
		resp, err := search.Indices.Count(ctx, &opensearchapi.IndicesCountReq{Indices: []string{index}, Body: strings.NewReader(q)})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Count
	}
	if got := entries(purged); got != 0 {
		t.Errorf("the purged board's comments: %d entries still findable; want 0", got)
	}
	if got := entries(kept); got != 1 {
		t.Errorf("the other board's comment: %d entries; want its 1 kept", got)
	}
}
