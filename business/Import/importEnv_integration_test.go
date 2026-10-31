//go:build integration

package business

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	globalSearchController "github.com/akashc777/OneCamp/controllers/GlobalSearch"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// importEnv starts what an import's stages write to: Postgres, the graph,
// Redis, a stubbed MQTT client, and a search server that takes every write.
func importEnv(t *testing.T) (*integration.Env, *integration.DgraphEnv) {
	t.Helper()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[],"result":"updated","_id":"x"}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	return env, dg
}

// seen is an import's world as people see and hear it: Postgres, Redis, the
// graph and a real search, with every realtime message and push the server
// sends kept as it goes out.
type seen struct {
	env    *integration.Env
	dg     *integration.DgraphEnv
	broker *keptBroker
	mu     sync.Mutex
	pushes []string
}

func seenWorld(t *testing.T) *seen {
	t.Helper()
	w := &seen{env: integration.SetupEnv(t), broker: &keptBroker{}}
	if err := postgresInit.ConnectPostgres(context.Background(), w.env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.UseMqttClient(t, w.broker)
	t.Cleanup(firebaseInit.ObservePushesForTest(func(data map[string]string, _ []string) {
		raw, _ := json.Marshal(data)
		w.mu.Lock()
		w.pushes = append(w.pushes, string(raw))
		w.mu.Unlock()
	}))
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

// pushed is every push sent so far, as JSON.
func (w *seen) pushed() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.pushes)
}

// keptBroker keeps every realtime message the server publishes.
type keptBroker struct {
	MQTT.Client
	mu  sync.Mutex
	got []string
}

func (b *keptBroker) Publish(topic string, _ byte, _ bool, payload interface{}) MQTT.Token {
	p, _ := payload.([]byte)
	b.mu.Lock()
	b.got = append(b.got, topic+" "+string(p))
	b.mu.Unlock()
	return keptToken{}
}

type keptToken struct{}

func (keptToken) Wait() bool                     { return true }
func (keptToken) WaitTimeout(time.Duration) bool { return true }
func (keptToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (keptToken) Error() error                   { return nil }

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
