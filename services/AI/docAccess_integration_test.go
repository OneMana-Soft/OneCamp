//go:build integration

package ai

// The AI search index, asked for real (the OpenSearch production runs): a
// private doc's entry and its comments' stay private through writes that
// don't know the doc's privacy (its text saved again, a comment edited), and
// SetDocAccess puts right entries that say otherwise and follows sharing.
// Run: go test -tags=integration ./services/AI/ -run TestDocAccessInTheIndex -v

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func TestDocAccessInTheIndex(t *testing.T) {
	ctx := context.Background()
	integration.SetupOpenSearch(t)
	if err := opensearchInit.RecreateAIEmbeddingsIndex(ctx, 3); err != nil {
		t.Fatal(err)
	}
	prev := servicePtr.Load()
	servicePtr.Store(enabledService(&fakeEmbedder{}))
	defer servicePtr.Store(prev)
	resetReindexBuffer()

	owner, reader, stranger := "owner-uuid", "reader-uuid", "stranger-uuid"
	yes, no := true, false
	store := func(doc EmbeddingDoc) {
		t.Helper()
		if err := StoreEmbedding(ctx, doc); err != nil {
			t.Fatalf("store %s:%s: %v", doc.ContentType, doc.ContentUUID, err)
		}
	}
	found := func(user string) string {
		t.Helper()
		if _, err := opensearchInit.OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{AI_EMBEDDINGS_INDEX}}); err != nil {
			t.Fatal(err)
		}
		body := `{"size": 50, "_source": ["content_uuid"], "query": {"bool": {"filter": [` +
			buildPermissionFilter(user, nil, nil, nil) + `]}}}`
		var resp struct {
			Hits struct {
				Hits []struct {
					Source struct {
						ContentUUID string `json:"content_uuid"`
					} `json:"_source"`
				} `json:"hits"`
			} `json:"hits"`
		}
		if _, err := opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
			Indices: []string{AI_EMBEDDINGS_INDEX}, Body: strings.NewReader(body),
		}, &resp); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, h := range resp.Hits.Hits {
			ids = append(ids, h.Source.ContentUUID)
		}
		sort.Strings(ids)
		return strings.Join(ids, ",")
	}
	expect := func(what, user, want string) {
		t.Helper()
		if got := found(user); got != want {
			t.Errorf("%s: %s finds [%s], want [%s]", what, user, got, want)
		}
	}
	setAccess := func(a DocAccess) {
		t.Helper()
		if err := SetDocAccess(ctx, []DocAccess{a}); err != nil {
			t.Fatal(err)
		}
	}

	// A private doc and a comment on it, written as the app writes them, and a
	// public doc.
	store(EmbeddingDoc{ContentText: "reorg plan", ContentType: "doc", ContentUUID: "plans",
		DocPrivate: &yes, DocCreatedByUserID: owner, DocEditingUsers: []string{owner}})
	store(EmbeddingDoc{ContentText: "agreed", ContentType: "comment", ContentUUID: "c1", DocUUID: "plans",
		DocPrivate: &yes, DocCreatedByUserID: owner, DocEditingUsers: []string{owner}})
	store(EmbeddingDoc{ContentText: "welcome", ContentType: "doc", ContentUUID: "handbook",
		DocPrivate: &no, DocCreatedByUserID: owner, DocEditingUsers: []string{owner}})
	expect("written", stranger, "handbook")
	expect("written", owner, "c1,handbook,plans")

	// Writes that don't know the privacy, into entries that exist: the doc's
	// text saved again, the comment edited. These made both public.
	store(EmbeddingDoc{ContentText: "reorg plan, v2", ContentType: "doc", ContentUUID: "plans"})
	store(EmbeddingDoc{ContentText: "agreed!", ContentType: "comment", ContentUUID: "c1"})
	expect("after saves that don't know the privacy", stranger, "handbook")
	expect("after saves that don't know the privacy", owner, "c1,handbook,plans")

	// An index an earlier version left: the private doc and its comment say
	// public. SetDocAccess (the backfill's write) puts them right.
	for _, id := range []string{"doc:plans", "comment:c1"} {
		if err := opensearchInit.UpdateDocument(ctx, AI_EMBEDDINGS_INDEX, id,
			openSearchStruct.IndexReader([]byte(`{"doc": {"doc_private": false}}`))); err != nil {
			t.Fatal(err)
		}
	}
	expect("as an earlier version left it", stranger, "c1,handbook,plans")
	setAccess(DocAccess{DocUUID: "plans", Private: true, CreatedBy: owner, Reading: []string{reader}, Editing: []string{owner}})
	expect("written right", stranger, "handbook")
	expect("written right", reader, "c1,handbook,plans")
	expect("written right", owner, "c1,handbook,plans")

	// No longer shared with the reader; then made public.
	setAccess(DocAccess{DocUUID: "plans", Private: true, CreatedBy: owner, Editing: []string{owner}})
	expect("unshared", reader, "handbook")
	setAccess(DocAccess{DocUUID: "plans", Private: false, CreatedBy: owner, Editing: []string{owner}})
	expect("made public", stranger, "c1,handbook,plans")

	// Another doc's entry is left as it was.
	resp, err := opensearchInit.OpenSearchClient.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: AI_EMBEDDINGS_INDEX, DocumentID: "doc:handbook"})
	if err != nil {
		t.Fatal(err)
	}
	var handbook EmbeddingDoc
	if err := json.Unmarshal(resp.Source, &handbook); err != nil {
		t.Fatal(err)
	}
	if handbook.DocPrivate == nil || *handbook.DocPrivate || handbook.DocCreatedByUserID != owner || len(handbook.DocEditingUsers) != 1 {
		t.Errorf("another doc's entry changed: %+v", handbook)
	}
}
