//go:build integration
// +build integration

package integration_test

// A private doc after its first edit, as AI search sees it (Postgres, Dgraph,
// Redis and OpenSearch, with a stand-in embedding model). Member A makes a
// private doc; the collaboration service saves its text (UpdateDocFromCollab,
// which carries the body alone). Member B's AI search must not find it, A's
// must, with its title still in what the index holds; making it public and
// private again is followed.
//
// Until 9 Oct a body-only save re-embedded the doc as public, so every
// member's Ask AI, Catch me up and /ai/search could quote it.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAPrivateDocStaysPrivateAfterItsFirstEdit -v

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

const privateDocEmbeddingDim = 8

// embedOnlyModel answers the embedding endpoint with the same vector for every
// text, so a k-NN search returns whatever its permission filter lets through.
type embedOnlyModel struct{}

func (embedOnlyModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/embed":
		var in struct {
			Input any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		n := 1
		if list, ok := in.Input.([]any); ok {
			n = len(list)
		}
		vecs := make([][]float32, n)
		for i := range vecs {
			vecs[i] = make([]float32, privateDocEmbeddingDim)
			vecs[i][0] = 1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vecs})
	case "/api/show":
		_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": []string{"completion"}})
	default:
		_, _ = io.Copy(io.Discard, r.Body)
		http.NotFound(w, r)
	}
}

func TestAPrivateDocStaysPrivateAfterItsFirstEdit(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	integration.SetupOpenSearch(t)

	model := httptest.NewServer(embedOnlyModel{})
	defer model.Close()
	t.Setenv("OLLAMA_HOST", model.URL)
	if _, err := env.PG.Exec(`UPDATE ai_settings SET embedding_dimension = $1, rate_limit_per_min = 1000`, privateDocEmbeddingDim); err != nil {
		t.Fatal(err)
	}
	if err := ai.InitAIService(); err != nil {
		t.Fatalf("init AI: %v", err)
	}
	if !ai.GetService().IsEnabled() {
		t.Fatal("AI did not come up enabled; nothing below would run")
	}
	t.Cleanup(func() {
		_, _ = env.PG.Exec(`UPDATE ai_settings SET enabled = false`)
		_ = ai.ReloadAIService(context.Background())
	})
	if err := opensearchInit.RecreateAIEmbeddingsIndex(ctx, privateDocEmbeddingDim); err != nil {
		t.Fatal(err)
	}

	a, b := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"ana": a, "ben": b} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	doc := uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": a.String(), "user_name": "ana"},
		{"uid": "_:ben", "dgraph.type": "User", "user_uuid": b.String(), "user_name": "ben"},
		{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "Q3 reorg", "doc_private": true,
			"doc_deleted_at": "0001-01-01T00:00:00Z", "doc_created_by": map[string]any{"uid": "_:ana"},
			"doc_editing_users": []map[string]any{{"uid": "_:ana"}}},
	})

	// The doc as CreateDoc indexes it: private, made by A, A its only editor.
	ai.EmbedDocContent("Q3 reorg", "", doc, a.String(), "ana", true, a.String(), nil, []string{a.String()}, nil)
	entry := func() map[string]any {
		t.Helper()
		var got struct {
			Found  bool           `json:"found"`
			Source map[string]any `json:"_source"`
		}
		resp, err := opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.DocumentGetReq{
			Index: ai.AI_EMBEDDINGS_INDEX, DocumentID: "doc:" + doc,
		}, &got)
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return got.Source
	}
	waitFor := func(what string, ok func(map[string]any) bool) {
		t.Helper()
		var last map[string]any
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if last = entry(); last != nil && ok(last) {
				return
			}
		}
		t.Fatalf("%s: the doc's entry is %v", what, last)
	}
	waitFor("created", func(e map[string]any) bool { return e["content_text"] != nil })

	// The first edit, as the collaboration service saves it: the body alone.
	if err := docBusiness.UpdateDocFromCollab(ctx, doc, "<p>zebra-quartz plan</p>", "", []string{a.String()}, false, ""); err != nil {
		t.Fatal(err)
	}
	waitFor("saved", func(e map[string]any) bool {
		s, _ := e["content_text"].(string)
		return strings.Contains(s, "zebra-quartz")
	})
	// Saves run in the background; give the last write (privacy) time to land.
	time.Sleep(time.Second)

	finds := func(user uuid.UUID) bool {
		t.Helper()
		if _, err := opensearchInit.OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{ai.AI_EMBEDDINGS_INDEX}}); err != nil {
			t.Fatal(err)
		}
		hits, err := ai.SearchSimilar(ctx, "zebra-quartz plan", user.String(), nil, nil, nil, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			if h.ContentUUID == doc {
				return true
			}
		}
		return false
	}
	if finds(b) {
		t.Errorf("after A's first edit, B's AI search finds A's private doc (entry: doc_private=%v)", entry()["doc_private"])
	}
	if !finds(a) {
		t.Errorf("A's AI search no longer finds A's own doc")
	}
	if s, _ := entry()["content_text"].(string); !strings.HasPrefix(s, "Q3 reorg") {
		t.Errorf("the saved entry lost the doc's title: %q", s)
	}

	// The owner makes it public, then private again; the index follows.
	waitFinds := func(what string, user uuid.UUID, want bool) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if finds(user) == want {
				return
			}
		}
		t.Errorf("%s: B finds the doc = %v, want %v", what, !want, want)
	}
	public, private := false, true
	if err := docBusiness.UpdateDoc(ctx, &docAdapter.InputUpdateDoc{DocId: doc, IsPrivate: &public}); err != nil {
		t.Fatal(err)
	}
	waitFinds("made public", b, true)
	if err := docBusiness.UpdateDoc(ctx, &docAdapter.InputUpdateDoc{DocId: doc, IsPrivate: &private}); err != nil {
		t.Fatal(err)
	}
	waitFinds("made private again", b, false)

	// And another body-only save of the now-private doc keeps it private.
	if err := docBusiness.UpdateDocFromCollab(ctx, doc, "<p>zebra-quartz plan, v2</p>", "", []string{a.String()}, false, ""); err != nil {
		t.Fatal(err)
	}
	waitFor("saved again", func(e map[string]any) bool {
		s, _ := e["content_text"].(string)
		return strings.Contains(s, "v2")
	})
	time.Sleep(time.Second)
	if finds(b) {
		t.Errorf("after a second edit, B's AI search finds the private doc again")
	}

	// A comments on the private doc. The comment's entry carries the doc's
	// sharing as the doc says it: A finds the comment, B doesn't. It used to be
	// written as shared with nobody and made by no one, so not even A could.
	var ana userModels.UserInfo
	ana.UserPostgresInfo.Id = a
	ana.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids["ana"], Uuid: a.String(), UserName: "ana"}
	raw, _ := json.Marshal(docAdapter.CreateOrUpdateDocCommentInput{DocUuid: doc, CommentBody: "<p>zebra-quartz agreed</p>"})
	rec := httptest.NewRecorder()
	docController.CreateDocComment(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).
		WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, ana)))
	if rec.Code != http.StatusOK {
		t.Fatalf("A's comment: %d %s", rec.Code, rec.Body.String())
	}
	findsComment := func(user uuid.UUID) bool {
		t.Helper()
		if _, err := opensearchInit.OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{ai.AI_EMBEDDINGS_INDEX}}); err != nil {
			t.Fatal(err)
		}
		hits, err := ai.SearchSimilar(ctx, "zebra-quartz agreed", user.String(), nil, nil, nil, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			if h.ContentType == "comment" && h.DocUUID == doc {
				return true
			}
		}
		return false
	}
	found := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline) && !found; time.Sleep(200 * time.Millisecond) {
		found = findsComment(a)
	}
	if !found {
		t.Errorf("A's AI search doesn't find A's own comment on A's private doc")
	}
	if findsComment(b) {
		t.Errorf("B's AI search finds a comment on A's private doc")
	}
}
