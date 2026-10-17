//go:build integration
// +build integration

package integration_test

// The public API's docs (C4) credit an agent (I11): a token bound to an agent
// makes and adds to docs as the agent, beside the token's owner, and its
// addition is kept as the agent's own edit with a version taken first; an
// unbound token's writes are its owner's alone. Real Postgres, Dgraph and
// Redis behind the token middleware; the live doc service is the one fake.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnAgentsTokenWritesDocsAsTheAgent -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/akashc777/OneCamp/tests/integration"
)

// liveCredits is the Live Doc Writer, keeping whom each first body and each
// addition was credited to.
type liveCredits struct {
	collab.Writer
	mu       sync.Mutex
	seeded   []collab.Options
	appended []collab.Options
}

func (l *liveCredits) Seed(_ context.Context, _, _ string, o collab.Options) (collab.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seeded = append(l.seeded, o)
	return collab.Result{}, nil
}

func (l *liveCredits) Append(_ context.Context, _, _ string, o collab.Options) (collab.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appended = append(l.appended, o)
	return collab.Result{Blocks: 1}, nil
}

func TestAnAgentsTokenWritesDocsAsTheAgent(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"result":"created"}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	live := &liveCredits{}
	savedLive := collab.Live
	collab.Live = live
	t.Cleanup(func() { collab.Live = savedLive })

	owner, _, err := userBusiness.CreateUserWithMethod(ctx, "scribe-owner@example.test", "Rosa", nil, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatalf("make the owner: %v", err)
	}
	agent := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by) VALUES ($1, 'Scribe', $2)`, agent, owner); err != nil {
		t.Fatal(err)
	}
	scopes := []string{apiTokenBusiness.ScopeDocsRead, apiTokenBusiness.ScopeDocsWrite}
	bound, err := apiTokenBusiness.CreateToken(ctx, "scribe", scopes, 30, owner, &agent, false)
	if err != nil {
		t.Fatalf("make the agent's token: %v", err)
	}
	plain, err := apiTokenBusiness.CreateToken(ctx, "notes script", scopes, 30, owner, nil, false)
	if err != nil {
		t.Fatalf("make a plain token: %v", err)
	}

	api := chi.NewRouter()
	api.Use(customMiddleware.VerifyApiToken)
	api.Post("/v1/docs", v1Controller.CreateDocV1)
	api.Post("/v1/docs/{id}/append", v1Controller.AppendToDocV1)
	call := func(token, path string, body any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code < 200 || rec.Code > 299 {
			t.Fatalf("POST %s answered %d: %v", path, rec.Code, out)
		}
		data, _ := out["data"].(map[string]any)
		return data
	}

	made := call(bound.Plaintext, "/v1/docs", map[string]any{"markdown": "# Standup notes\n\nShipped the importer."})
	docID, _ := made["uuid"].(string)
	if docID == "" || made["title"] != "Standup notes" {
		t.Fatalf("the agent's new doc came back as %v", made)
	}
	call(bound.Plaintext, "/v1/docs/"+docID+"/append", map[string]any{"markdown": "Next: the exporter."})
	call(plain.Plaintext, "/v1/docs/"+docID+"/append", map[string]any{"markdown": "Rosa: agreed."})

	ownerID, agentID := owner.String(), agent.String()
	if len(live.seeded) != 1 || live.seeded[0].By != (collab.By{UserUUID: ownerID, AgentUUID: agentID}) {
		t.Errorf("the new doc's first body was credited %+v, want Rosa with the Scribe", live.seeded)
	}
	if len(live.appended) != 2 {
		t.Fatalf("%d additions reached the doc, want 2", len(live.appended))
	}
	if a := live.appended[0]; a.By != (collab.By{UserUUID: ownerID, AgentUUID: agentID}) || !a.AgentEdit || !a.SnapshotFirst {
		t.Errorf("the agent's addition went as %+v, want the Scribe's own edit with a version kept first", a)
	}
	if p := live.appended[1]; p.By != (collab.By{UserUUID: ownerID}) || p.AgentEdit || p.SnapshotFirst {
		t.Errorf("the plain token's addition went as %+v, want Rosa's alone", p)
	}
}
