//go:build integration
// +build integration

package integration_test

// A comment hidden by its own marker (business/Comment HideComment: an old
// import's re-check hides a comment its source keeps from some) leaves search
// under that marker, in each index that names it: over a cascade's mark, so
// its task's or project's restore can't bring it back, and never over a
// person's delete. The hide's script didn't compile, so every hide failed and
// an import with a kept comment never settled. Against a real OpenSearch with
// the indices' own mappings.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAHiddenCommentLeavesSearchUnderItsOwnMark -v

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	globalSearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAHiddenCommentLeavesSearchUnderItsOwnMark(t *testing.T) {
	ctx := context.Background()
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	client := opensearchInit.OpenSearchClient
	const before, at = int64(1700000000), int64(1760000000)

	// A comment's entry in each index, by the field that names it there.
	entries := map[string]func(comment string) map[string]any{
		"comments": func(comment string) map[string]any {
			return map[string]any{"comment_id": comment, "comment_body": "Yarrow supplier rates"}
		},
		"attachments": func(comment string) map[string]any {
			return map[string]any{"attachment_id": uuid.NewString(), "attachment_comment_id": comment, "attachment_file_name": "rates.pdf"}
		},
		"ai_embeddings": func(comment string) map[string]any {
			return map[string]any{"content_type": "comment", "content_uuid": comment, "content_text": "Yarrow supplier rates"}
		},
	}
	// How a comment's entries stand before the hide: live, out with its
	// trashed task or archived project, or deleted by a person.
	states := map[string]map[string]any{
		"live":    {},
		"cascade": {"deleted_date": before, "deleted_by": "cascade"},
		"user":    {"deleted_date": before, "deleted_by": "user"},
	}
	type mark struct {
		at int64
		by any
	}
	want := map[string]mark{"live": {at, "recheck"}, "cascade": {at, "recheck"}, "user": {before, "user"}}

	fields := []string{"comment_id", "attachment_comment_id", "content_uuid"}
	indices := []string{"comments", "attachments", "ai_embeddings"}
	write := func(index, id string, body map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		if _, err := client.Index(ctx, opensearchapi.IndexReq{Index: index, DocumentID: id, Body: strings.NewReader(string(raw)),
			Params: opensearchapi.IndexParams{Refresh: "true"}}); err != nil {
			t.Fatalf("index %s/%s: %v", index, id, err)
		}
	}
	markOf := func(index, id string) mark {
		t.Helper()
		resp, err := client.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: index, DocumentID: id})
		if err != nil {
			t.Fatalf("read %s/%s: %v", index, id, err)
		}
		raw, _ := io.ReadAll(resp.Inspect().Response.Body)
		var got struct {
			Source map[string]any `json:"_source"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		d, _ := got.Source["deleted_date"].(float64)
		return mark{int64(d), got.Source["deleted_by"]}
	}

	// Each state's comment, and another comment beside it in each index.
	comments := map[string]string{}
	other := uuid.NewString()
	for state, fieldsOf := range states {
		comments[state] = uuid.NewString()
		for index, entry := range entries {
			body := entry(comments[state])
			for k, v := range fieldsOf {
				body[k] = v
			}
			write(index, state+"-"+comments[state], body)
		}
	}
	for index, entry := range entries {
		write(index, "other-"+other, entry(other))
	}

	for state, comment := range comments {
		if err := globalSearchModels.HideInOpenSearch(ctx, fields, comment, at, indices, "recheck"); err != nil {
			t.Fatalf("hiding the %s comment: %v", state, err)
		}
	}
	for state, comment := range comments {
		for _, index := range indices {
			if got := markOf(index, state+"-"+comment); got != want[state] {
				t.Errorf("the %s comment's %s entry after its hide: %+v; want %+v", state, index, got, want[state])
			}
		}
	}
	for _, index := range indices {
		if got := markOf(index, "other-"+other); got != (mark{0, nil}) {
			t.Errorf("another comment's %s entry after the hides: %+v; want it live", index, got)
		}
	}
}
