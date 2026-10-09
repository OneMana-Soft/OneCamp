//go:build integration
// +build integration

package integration_test

// The one-time rewrite of private docs' privacy onto their AI search entries
// (business/Doc.RunEmbeddingAccessBackfill), against Postgres, Dgraph and
// OpenSearch: entries an earlier version left saying "public" are put right
// from the docs, a doc's comments with it, a public doc isn't touched, and
// once it has run it doesn't run again.
//
// Run: go test -tags=integration ./tests/integration/ -run TestPrivateDocsEmbeddingBackfill -v

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestPrivateDocsEmbeddingBackfill(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	integration.SetupOpenSearch(t)
	if err := opensearchInit.RecreateAIEmbeddingsIndex(ctx, 3); err != nil {
		t.Fatal(err)
	}

	owner, reader, stranger := uuid.New(), uuid.New(), uuid.NewString()
	plans, notes, handbook, comment := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.New()
	for name, id := range map[string]uuid.UUID{"owner": owner, "reader": reader} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO comments (id, created_by) VALUES ($1, $2)`, comment, owner); err != nil {
		t.Fatal(err)
	}
	live := "0001-01-01T00:00:00Z"
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": owner.String(), "user_name": "owner"},
		{"uid": "_:reader", "dgraph.type": "User", "user_uuid": reader.String(), "user_name": "reader"},
		{"uid": "_:plans", "dgraph.type": "Doc", "doc_uuid": plans, "doc_title": "plans", "doc_private": true,
			"doc_deleted_at": live, "doc_created_by": map[string]any{"uid": "_:owner"},
			"doc_editing_users": []map[string]any{{"uid": "_:owner"}},
			"doc_reading_users": []map[string]any{{"uid": "_:reader"}}},
		{"uid": "_:notes", "dgraph.type": "Doc", "doc_uuid": notes, "doc_title": "notes", "doc_private": true,
			"doc_deleted_at": live, "doc_created_by": map[string]any{"uid": "_:owner"},
			"doc_editing_users": []map[string]any{{"uid": "_:owner"}}},
		{"uid": "_:handbook", "dgraph.type": "Doc", "doc_uuid": handbook, "doc_title": "handbook", "doc_private": false,
			"doc_deleted_at": live, "doc_created_by": map[string]any{"uid": "_:owner"},
			"doc_editing_users": []map[string]any{{"uid": "_:owner"}}},
	})

	// The index as an earlier version left it: every entry says public.
	now := time.Now().Unix()
	for id, entry := range map[string]map[string]any{
		"doc:" + plans:                {"content_type": "doc", "content_uuid": plans},
		"doc:" + notes:                {"content_type": "doc", "content_uuid": notes},
		"doc:" + handbook:             {"content_type": "doc", "content_uuid": handbook},
		"comment:" + comment.String(): {"content_type": "comment", "content_uuid": comment.String(), "doc_uuid": plans},
	} {
		entry["content_text"] = "words"
		entry["created_date"] = now
		entry["doc_private"] = false
		entry["doc_created_by_user_id"] = owner.String()
		entry["doc_editing_users"] = []string{owner.String()}
		raw, _ := json.Marshal(entry)
		if _, err := opensearchInit.OpenSearchClient.Document.Create(ctx, opensearchapi.DocumentCreateReq{
			Index: ai.AI_EMBEDDINGS_INDEX, DocumentID: id, Body: strings.NewReader(string(raw)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	found := func(user string) string {
		t.Helper()
		if _, err := opensearchInit.OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{ai.AI_EMBEDDINGS_INDEX}}); err != nil {
			t.Fatal(err)
		}
		hits, err := ai.SearchRecentGlobal(ctx, user, nil, nil, nil, 50)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]string{plans: "plans", notes: "notes", handbook: "handbook", comment.String(): "comment"}
		var got []string
		for _, h := range hits {
			got = append(got, names[h.ContentUUID])
		}
		sort.Strings(got)
		return strings.Join(got, ",")
	}

	if got := found(stranger); got != "comment,handbook,notes,plans" {
		t.Fatalf("before: someone else finds [%s]; the test isn't starting from the leak", got)
	}

	written, err := docBusiness.RunEmbeddingAccessBackfill(ctx)
	if err != nil || written != 2 {
		t.Fatalf("backfill wrote %d private docs (%v), want 2", written, err)
	}
	for who, want := range map[string]string{
		stranger:        "handbook",
		reader.String(): "comment,handbook,plans",
		owner.String():  "comment,handbook,notes,plans",
	} {
		if got := found(who); got != want {
			t.Errorf("after: %s finds [%s], want [%s]", who, got, want)
		}
	}

	var state string
	if err := env.PG.QueryRow(`SELECT value FROM system_configs WHERE key = 'ai_embeddings_doc_access_v1'`).Scan(&state); err != nil || state != "done" {
		t.Errorf("the backfill didn't record that it ran: %q %v", state, err)
	}
	if again, err := docBusiness.RunEmbeddingAccessBackfill(ctx); err != nil || again != 0 {
		t.Errorf("a second run wrote %d docs (%v), want none", again, err)
	}

	// From here, the live path: the owner changes who may see the doc, and
	// its entries follow (in the background, as the app does it).
	waitFound := func(what, who, want string) {
		t.Helper()
		got := ""
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if got = found(who); got == want {
				return
			}
		}
		t.Errorf("%s: %s finds [%s], want [%s]", what, who, got, want)
	}
	public, private := false, true
	if err := docBusiness.UpdateDoc(ctx, &docAdapter.InputUpdateDoc{DocId: plans, IsPrivate: &public}); err != nil {
		t.Fatal(err)
	}
	waitFound("made public", stranger, "comment,handbook,plans")
	if err := docBusiness.UpdateDoc(ctx, &docAdapter.InputUpdateDoc{DocId: plans, IsPrivate: &private}); err != nil {
		t.Fatal(err)
	}
	waitFound("made private again", stranger, "handbook")
	if err := docBusiness.UpdateDocPermissions(ctx, docAdapter.InputUpdateDocPermissions{
		DocId: plans, RemoveViewers: []string{reader.String()},
	}, uids["owner"]); err != nil {
		t.Fatal(err)
	}
	waitFound("no longer shared with them", reader.String(), "handbook")
}
