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
