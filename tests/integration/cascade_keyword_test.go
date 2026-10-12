//go:build integration
// +build integration

package integration_test

// Deleting a doc marks its comments' AI search entries deleted too.
//
// The cascade matched them with a term query on doc_uuid, a field the AI index
// maps by itself as analyzed text, so a whole uuid never matched and the
// comments stayed findable in AI search after their doc was deleted. Against a
// real OpenSearch with the index's own mapping.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDeletingADocTakesItsCommentsOutOfAISearch -v

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
	globalSearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestDeletingADocTakesItsCommentsOutOfAISearch(t *testing.T) {
	ctx := context.Background()
	integration.SetupOpenSearch(t)
	if err := opensearchInit.RecreateAIEmbeddingsIndex(ctx, 3); err != nil {
		t.Fatal(err)
	}
	client := opensearchInit.OpenSearchClient
	doc, comment, other := uuid.NewString(), uuid.NewString(), uuid.NewString()
	index := func(id string, body map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		if _, err := client.Index(ctx, opensearchapi.IndexReq{Index: "ai_embeddings", DocumentID: id, Body: strings.NewReader(string(raw))}); err != nil {
			t.Fatal(err)
		}
	}
	index("doc-"+doc, map[string]any{"content_type": "doc", "content_uuid": doc, "content_text": "Q4 launch plan"})
	index("comment-"+comment, map[string]any{"content_type": "doc_comment", "content_uuid": comment, "doc_uuid": doc, "content_text": "the load test is still red"})
	index("comment-"+other, map[string]any{"content_type": "doc_comment", "content_uuid": other, "doc_uuid": uuid.NewString(), "content_text": "on another doc"})
	refresh := func() {
		t.Helper()
		if _, err := client.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{"ai_embeddings"}}); err != nil {
			t.Fatal(err)
		}
	}
	refresh()

	if err := globalSearchModels.SyncCascadingDeletionInOpenSearch(ctx, []string{"content_uuid", "doc_uuid"}, doc, time.Now().Unix(), []string{"ai_embeddings"}, "cascade"); err != nil {
		t.Fatal(err)
	}
	refresh()

	deleted := func(id string) bool {
		t.Helper()
		resp, err := client.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: "ai_embeddings", DocumentID: id})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Inspect().Response.Body)
		var got struct {
			Source map[string]any `json:"_source"`
		}
		_ = json.Unmarshal(raw, &got)
		_, ok := got.Source["deleted_date"]
		return ok
	}
	if !deleted("doc-" + doc) {
		t.Error("the doc's own entry wasn't marked deleted")
	}
	if !deleted("comment-" + comment) {
		t.Error("a comment on the deleted doc stays in AI search")
	}
	if deleted("comment-" + other) {
		t.Error("a comment on another doc was marked deleted")
	}
}

// A cascade marks what a parent going away takes with it ("cascade"), and
// bringing the parent back clears what the cascade marked. An entry deleted
// before the cascade (a comment its author deleted: its own entry has no
// marker, its AI entry "user") keeps its deletion time and marker through
// both: the cascade wrote its own over them, so restoring a doc brought its
// deleted comments back to search. An entry written as deleted at -1 (a doc's
// new comments once were, and a team's restore still writes it) was never
// deleted, so it is marked, and brought back, like a live one. Each of the
// three cascades, against a real OpenSearch with the indices' own mappings.
//
// Run: go test -tags=integration ./tests/integration/ -run TestACascadeLeavesWhatWasDeletedBeforeItAsItWas -v
func TestACascadeLeavesWhatWasDeletedBeforeItAsItWas(t *testing.T) {
	ctx := context.Background()
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	if err := opensearchInit.RecreateAIEmbeddingsIndex(ctx, 3); err != nil {
		t.Fatal(err)
	}
	client := opensearchInit.OpenSearchClient
	const before, at = int64(1700000000), int64(1760000000)

	// What each parent has in its index, by how it was written.
	kinds := map[string]map[string]any{
		"live":   {},
		"null":   {"deleted_date": nil},
		"-1":     {"deleted_date": -1},
		"author": {"deleted_date": before},
		"user":   {"deleted_date": before, "deleted_by": "user"},
	}
	type cascade struct {
		index   string
		entry   func(parent, id string) map[string]any
		run     func(parent string)
		restore func(parent string)
	}
	cascades := map[string]cascade{
		"one parent, several fields": {
			index: "comments",
			entry: func(parent, id string) map[string]any {
				return map[string]any{"comment_id": id, "comment_doc_id": parent, "comment_body": "the load test is still red"}
			},
			run: func(parent string) {
				if err := globalSearchModels.SyncCascadingDeletionInOpenSearch(ctx, []string{"comment_doc_id", "attachment_doc_id"}, parent, at, []string{"comments"}, "cascade"); err != nil {
					t.Fatal(err)
				}
			},
			restore: func(parent string) {
				if err := globalSearchModels.SyncCascadingUnarchiveInOpenSearch(ctx, []string{"comment_doc_id", "attachment_doc_id"}, parent, []string{"comments"}, "cascade"); err != nil {
					t.Fatal(err)
				}
			},
		},
		"one field, several ids": {
			index: "attachments",
			entry: func(parent, id string) map[string]any {
				return map[string]any{"attachment_id": id, "attachment_doc_id": parent, "attachment_file_name": "plan.pdf"}
			},
			run: func(parent string) {
				_ = globalSearchModels.SyncCascadingDeletionInOpenSearchMulti(ctx, "attachment_doc_id", []string{parent}, at, []string{"attachments"}, "cascade")
			},
			restore: func(parent string) {
				_ = globalSearchModels.SyncCascadingUnarchiveInOpenSearchMulti(ctx, "attachment_doc_id", []string{parent}, []string{"attachments"}, "cascade")
			},
		},
		"several fields and ids": {
			index: "ai_embeddings",
			entry: func(parent, id string) map[string]any {
				return map[string]any{"content_type": "comment", "content_uuid": id, "doc_uuid": parent, "content_text": "the load test is still red"}
			},
			run: func(parent string) {
				_ = globalSearchModels.SyncCascadingDeletionInOpenSearchCombined(ctx, map[string][]string{"content_uuid": {parent}, "doc_uuid": {parent}}, at, []string{"ai_embeddings"}, "cascade")
			},
			restore: func(parent string) {
				_ = globalSearchModels.SyncCascadingUnarchiveInOpenSearchCombined(ctx, map[string][]string{"content_uuid": {parent}, "doc_uuid": {parent}}, []string{"ai_embeddings"}, "cascade")
			},
		},
	}

	source := func(index, id string) map[string]any {
		t.Helper()
		resp, err := client.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: index, DocumentID: id})
		if err != nil {
			t.Fatalf("read %s/%s: %v", index, id, err)
		}
		raw, _ := io.ReadAll(resp.Inspect().Response.Body)
		var got struct {
			Source map[string]any `json:"_source"`
		}
		_ = json.Unmarshal(raw, &got)
		return got.Source
	}
	// marks is an entry's deletion time (0: none) and marker.
	marks := func(e map[string]any) (int64, any) {
		d, _ := e["deleted_date"].(float64)
		return int64(d), e["deleted_by"]
	}

	for name, c := range cascades {
		parent := uuid.NewString()
		ids := map[string]string{}
		for kind, fields := range kinds {
			ids[kind] = uuid.NewString()
			body := c.entry(parent, ids[kind])
			for k, v := range fields {
				body[k] = v
			}
			raw, _ := json.Marshal(body)
			if _, err := client.Index(ctx, opensearchapi.IndexReq{Index: c.index, DocumentID: ids[kind], Body: strings.NewReader(string(raw)),
				Params: opensearchapi.IndexParams{Refresh: "true"}}); err != nil {
				t.Fatalf("%s: index the %s entry: %v", name, kind, err)
			}
		}
		check := func(when string, want map[string][2]any) {
			t.Helper()
			for kind, w := range want {
				d, by := marks(source(c.index, ids[kind]))
				if d != w[0].(int64) || by != w[1] {
					t.Errorf("%s (%s), after %s: the %s entry is deleted at %d by %v, want at %d by %v", name, c.index, when, kind, d, by, w[0], w[1])
				}
			}
		}

		c.run(parent)
		check("the cascade", map[string][2]any{
			"live": {at, "cascade"}, "null": {at, "cascade"}, "-1": {at, "cascade"},
			"author": {before, nil}, "user": {before, "user"},
		})
		c.restore(parent)
		check("the restore", map[string][2]any{
			"live": {int64(0), nil}, "null": {int64(0), nil}, "-1": {int64(0), nil},
			"author": {before, nil}, "user": {before, "user"},
		})
	}
}
