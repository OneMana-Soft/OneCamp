//go:build integration

package business

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	globalSearchController "github.com/akashc777/OneCamp/controllers/GlobalSearch"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/akashc777/OneCamp/tests/integration"
)

// seen is an import's world as people see and hear it: Postgres, Redis, the
// graph and a real search, with every realtime message the server sends kept
// as it goes out (keptBroker).
type seen struct {
	env    *integration.Env
	dg     *integration.DgraphEnv
	broker *keptBroker
}

func seenWorld(t *testing.T) *seen {
	t.Helper()
	w := &seen{env: integration.SetupEnv(t), broker: &keptBroker{}}
	if err := postgresInit.ConnectPostgres(context.Background(), w.env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.UseMqttClient(t, w.broker)
	w.dg = integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	return w
}

// live is every realtime message sent so far, as "topic payload".
func (w *seen) live() []string {
	w.broker.mu.Lock()
	defer w.broker.mu.Unlock()
	return slices.Clone(w.broker.got)
}

// signedIn is ctx with id signed in, as the first middleware leaves a request.
func signedIn(t *testing.T, ctx context.Context, id uuid.UUID) context.Context {
	t.Helper()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
	if err != nil || pg == nil {
		t.Fatalf("load %s: %v", id, err)
	}
	gr, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, id.String())
	if err != nil || gr == nil {
		t.Fatalf("load %s's profile: %v", id, err)
	}
	return taskread.Install(context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}))
}

// callAs is h's answer to id (its status and body), given the route's
// params, a query string and a body sent as JSON.
func callAs(t *testing.T, ctx context.Context, id uuid.UUID, h http.HandlerFunc, params map[string]string, query string, body any) (int, string) {
	t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/"+query, rd).WithContext(context.WithValue(signedIn(t, ctx, id), chi.RouteCtxKey, rc)))
	return rec.Code, rec.Body.String()
}

// searchMarks is the deleted_by of each search entry naming a comment, as
// HideComment marks them: its own entry, its files' and its AI entry, each
// matched whole (by the field or its .keyword), in whichever of those indexes
// the install has.
func searchMarks(ctx context.Context, comment string) ([]string, error) {
	var should []map[string]any
	for _, field := range []string{"comment_id", "attachment_comment_id", "content_uuid"} {
		for _, f := range []string{field, field + ".keyword"} {
			should = append(should, map[string]any{"term": map[string]any{f: comment}})
		}
	}
	body, err := json.Marshal(map[string]any{"size": 100,
		"query": map[string]any{"bool": map[string]any{"should": should, "minimum_should_match": 1}}})
	if err != nil {
		return nil, err
	}
	ignore := true
	resp, err := opensearchInit.OpenSearchClient.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{"comments", "attachments", "ai_embeddings"}, Body: bytes.NewReader(body),
		Params: opensearchapi.SearchParams{IgnoreUnavailable: &ignore}})
	if err != nil {
		return nil, err
	}
	marks := make([]string, len(resp.Hits.Hits))
	for i, h := range resp.Hits.Hits {
		var e struct {
			By string `json:"deleted_by"`
		}
		if err := json.Unmarshal(h.Source, &e); err != nil {
			return nil, err
		}
		marks[i] = e.By
	}
	return marks, nil
}

// searchAs is id's global search for text: its answer, and how many hits its
// page holds.
func searchAs(t *testing.T, ctx context.Context, id uuid.UUID, text string) (string, int) {
	t.Helper()
	_, out := callAs(t, ctx, id, globalSearchController.GetUnifiedGlobalSearch, nil, "", map[string]string{"global_search_text": text})
	var page struct {
		Data struct {
			Page []json.RawMessage `json:"page"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &page)
	return out, len(page.Data.Page)
}
