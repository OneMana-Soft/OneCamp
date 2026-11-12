//go:build integration
// +build integration

package integration_test

// V-528 (1): a block the collaboration service kept as a marker, because it
// was a release behind the web when the block was written (deploys go web
// first; preserved.js keeps what its schema lacks), is in what the public
// API's GET answers: from the live doc, and, with the live service away, from
// the body the service last saved. Real Postgres, Dgraph and Redis behind the
// token middleware; the live doc service is the fake.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheAPIsGetKeepsABlockTheServiceKept -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheAPIsGetKeepsABlockTheServiceKept(t *testing.T) {
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
	savedLive, savedRead := collab.Live, collab.Read
	collab.Live = &liveCredits{}
	t.Cleanup(func() { collab.Live, collab.Read = savedLive, savedRead })

	owner, _, err := userBusiness.CreateUserWithMethod(ctx, "kept-block@example.test", "Rosa", nil, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatalf("make the owner: %v", err)
	}
	token, err := apiTokenBusiness.CreateToken(ctx, "notes script", []string{apiTokenBusiness.ScopeDocsRead, apiTokenBusiness.ScopeDocsWrite}, 30, owner, nil, false)
	if err != nil {
		t.Fatalf("make the token: %v", err)
	}

	api := chi.NewRouter()
	api.Use(customMiddleware.VerifyApiToken)
	api.Post("/v1/docs", v1Controller.CreateDocV1)
	api.Get("/v1/docs/{id}", v1Controller.GetDocV1)
	call := func(method, path string, body any) map[string]any {
		t.Helper()
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("Authorization", "Bearer "+token.Plaintext)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code < 200 || rec.Code > 299 {
			t.Fatalf("%s %s answered %d: %v", method, path, rec.Code, out)
		}
		data, _ := out["data"].(map[string]any)
		return data
	}

	made := call(http.MethodPost, "/v1/docs", map[string]any{"markdown": "# Plan\n\nBefore"})
	docID, _ := made["uuid"].(string)
	if docID == "" {
		t.Fatalf("the new doc came back as %v", made)
	}
	const board = "0b0e5a8e-3c1d-4f6a-9b2e-7d4c5a6b8c9d"
	// Beside the whiteboard, a picture whose address its tag twin would refuse
	// (CR-220 F1): it reads as its alt text, never as the address.
	kept := `<p>Before</p><div data-oc-preserved="whiteboardEmbed" data-oc-holds="none" data-oc-attrs="{&quot;boardId&quot;:&quot;` + board + `&quot;}"></div>` +
		`<div data-oc-preserved="image" data-oc-holds="none" data-oc-attrs="{&quot;src&quot;:&quot;javascript:alert(1)&quot;,&quot;alt&quot;:&quot;A cat&quot;}"></div><p>After</p>`
	refused := func(when, md string) {
		if strings.Contains(md, "javascript:") || !strings.Contains(md, "A cat") {
			t.Errorf("GET from the %s carries the refused picture's address, or loses its words:\n%s", when, md)
		}
	}

	// The live doc, from a service a release behind.
	collab.Read = func(context.Context, string) (collab.Current, error) {
		return collab.Current{HTML: kept}, nil
	}
	if md, _ := call(http.MethodGet, "/v1/docs/"+docID, nil)["markdown"].(string); !strings.Contains(md, "/app/board/"+board) || !strings.Contains(md, "After") {
		t.Errorf("GET from the live doc leaves the whiteboard out:\n%s", md)
	} else {
		refused("live doc", md)
	}

	// The live service away: the body it last saved.
	collab.Read = func(context.Context, string) (collab.Current, error) {
		return collab.Current{}, collab.ErrUnavailable
	}
	if _, err := docBusiness.SaveFromCollab(ctx, docBusiness.CollabSave{DocUUID: docID, HTML: kept, Quiet: true}); err != nil {
		t.Fatalf("the service's save: %v", err)
	}
	if md, _ := call(http.MethodGet, "/v1/docs/"+docID, nil)["markdown"].(string); !strings.Contains(md, "/app/board/"+board) || !strings.Contains(md, "After") {
		t.Errorf("GET from the saved body leaves the whiteboard out:\n%s", md)
	} else {
		refused("saved body", md)
	}
}
