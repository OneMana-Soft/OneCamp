//go:build integration
// +build integration

package integration_test

// A docs list page asks the Access Resolver in batches, never once per row:
// with each row's level added (docs incident, 14 Oct 2026), a page of 100 of
// a few hundred docs still costs at most two batched reads (the rows' guard,
// and the rows' levels with the pages above them, in one), and no single-doc
// read at all. The time a page takes is logged, to compare before and after.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDocListAsksInBatches -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// countingRules counts the resolver's reads: one-doc reads, and batched ones.
type countingRules struct {
	accessBusiness.Resolver
	single, batched *int
}

func (c countingRules) Resolve(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
	*c.single++
	return c.Resolver.Resolve(ctx, who, res)
}

func (c countingRules) Filter(ctx context.Context, who accessBusiness.Subject, res []accessBusiness.Resource, atLeast accessBusiness.Level) ([]accessBusiness.Resource, error) {
	*c.batched++
	return c.Resolver.Filter(ctx, who, res, atLeast)
}

func (c countingRules) ResolveAll(ctx context.Context, who accessBusiness.Subject, res []accessBusiness.Resource) (map[accessBusiness.Resource]accessBusiness.Decision, error) {
	*c.batched++
	return c.Resolver.(accessBusiness.Batch).ResolveAll(ctx, who, res)
}

func TestDocListAsksInBatches(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	owner, reader := uuid.New(), uuid.New()
	for n, id := range map[string]uuid.UUID{"owner": owner, "reader": reader} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			id, n+"@example.test", n, n); err != nil {
			t.Fatal(err)
		}
	}
	nodes := []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": owner.String(), "user_name": "owner"},
		{"uid": "_:reader", "dgraph.type": "User", "user_uuid": reader.String(), "user_name": "reader"},
	}
	// 400 workspace docs: 300 at the top, and 100 under ten of them.
	base := time.Date(2026, 10, 14, 5, 0, 0, 0, time.UTC)
	for i := 0; i < 400; i++ {
		d := map[string]any{"uid": fmt.Sprintf("_:d%d", i), "dgraph.type": "Doc", "doc_uuid": uuid.NewString(), "doc_title": fmt.Sprintf("Doc %03d", i),
			"doc_private": false, "doc_public_comment": i%3 == 0, "doc_deleted_at": "0001-01-01T00:00:00Z",
			"doc_created_at": base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339), "doc_updated_at": base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			"doc_created_by": map[string]any{"uid": "_:owner"}}
		if i >= 300 {
			d["doc_parent"] = map[string]any{"uid": fmt.Sprintf("_:d%d", i%10)}
		}
		nodes = append(nodes, d)
	}
	uids := dg.Mutate(t, nodes)

	var u userModels.UserInfo
	u.UserPostgresInfo.Id = reader
	u.UserPostgresInfo.EmailID = "reader@example.test"
	u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids["reader"], Uuid: reader.String(), UserName: "reader"}
	page := func(cursor string) (answer, time.Duration) {
		target := "/doc/list?scope=workspace&limit=100"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		req := httptest.NewRequest("GET", target, nil)
		c := context.WithValue(ctx, helpers.UserInfoContextKey, u)
		req = req.WithContext(context.WithValue(c, chi.RouteCtxKey, chi.NewRouteContext()))
		rec := httptest.NewRecorder()
		start := time.Now()
		docController.GetDocList(rec, req)
		took := time.Since(start)
		out := answer{code: rec.Code}
		_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
		return out, took
	}

	var single, batched int
	was := accessBusiness.Rules
	accessBusiness.Rules = countingRules{Resolver: was, single: &single, batched: &batched}
	defer func() { accessBusiness.Rules = was }()

	var times []time.Duration
	for run := 0; run < 5; run++ {
		cursor, rows, total := "", 0, time.Duration(0)
		for pages := 0; pages < 10; pages++ {
			single, batched = 0, 0
			a, took := page(cursor)
			if a.code != 200 {
				t.Fatalf("list: %d %v", a.code, a.body)
			}
			total += took
			docs, _ := a.data()["docs"].([]any)
			rows += len(docs)
			if single != 0 || batched > 2 {
				t.Errorf("a page of %d rows asked the resolver %d times one doc at a time and %d times in a batch; want 0 and at most 2", len(docs), single, batched)
			}
			cursor, _ = a.data()["next_cursor"].(string)
			if cursor == "" {
				break
			}
		}
		if rows != 400 {
			t.Fatalf("listed %d docs, want 400", rows)
		}
		times = append(times, total)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	t.Logf("400 docs in pages of 100: median %v for the whole list (fastest %v, slowest %v)", times[2], times[0], times[4])
}
