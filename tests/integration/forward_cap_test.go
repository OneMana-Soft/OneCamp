//go:build integration
// +build integration

package integration_test

// A forward names at most 100 places (business/Send MaxForwardPlaces): a
// longer list is refused in words that say the limit, before anything is
// read or written. Its groups and people are read at once
// (chatBusiness.StandingsOf), so a group and a person in one request are
// each checked and each written. Against real Postgres, Redis and Dgraph.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAForwardNamesAtMost100Places -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAForwardNamesAtMost100Places(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Ben is in a group with Cy; Ana is in neither.
	ana, ben, cy := uuid.New(), uuid.New(), uuid.New()
	benGroup := "grp-" + uuid.NewString()
	for name, id := range map[string]uuid.UUID{"ana": ana, "ben": ben, "cy": cy} {
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
	}
	got := dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": ana.String(), "user_name": "ana"},
		{"uid": "_:ben", "dgraph.type": "User", "user_uuid": ben.String(), "user_name": "ben"},
		{"uid": "_:cy", "dgraph.type": "User", "user_uuid": cy.String(), "user_name": "cy"},
		{"uid": "_:bengroup", "dgraph.type": "Dm", "dm_grouping_id": benGroup, "dm_participants": []map[string]any{{"uid": "_:ben"}, {"uid": "_:cy"}}},
	})

	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, ben)
	if err != nil || pg == nil {
		t.Fatalf("load ben: %v", err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, ben.String())
	if err != nil || gr == nil {
		t.Fatalf("load ben's node: %v", err)
	}
	asBen := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	forward := func(to []map[string]any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"fwd_text": "<p>fyi</p>", "fwd_list": to})
		rec := httptest.NewRecorder()
		userController.FwdUserMessage(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(asBen))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	count := func(q string) int {
		t.Helper()
		var out map[string][]map[string]int
		if err := json.Unmarshal(dg.Query(t, q), &out); err != nil || len(out["q"]) != 1 {
			t.Fatalf("%s: %v %v", q, out, err)
		}
		return out["q"][0]["n"]
	}
	groupHolds := func() int {
		return count(fmt.Sprintf(`{ q(func: uid(%s)) { n: count(dm_chats) } }`, got["bengroup"]))
	}
	dmWithCy := func() int {
		var out map[string][]map[string]int
		_ = json.Unmarshal(dg.Query(t, fmt.Sprintf(`{ q(func: eq(dm_grouping_id, %q)) { n: count(dm_chats) } }`, helpers.GetGroupingId(ben.String(), cy.String()))), &out)
		if len(out["q"]) == 0 {
			return 0
		}
		return out["q"][0]["n"]
	}

	// 101 places: refused in words with the limit, and nothing written.
	places := []map[string]any{{"grp_id": benGroup}}
	for len(places) < sendBusiness.MaxForwardPlaces+1 {
		places = append(places, map[string]any{"user_uuid": cy.String()})
	}
	code, body := forward(places)
	if code != http.StatusBadRequest {
		t.Fatalf("a forward to 101 places: %d, want 400", code)
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "at most 100 places") {
		t.Errorf("the refusal says %q, want the limit in words", msg)
	}
	if limit, _ := body["limit"].(float64); int(limit) != 100 {
		t.Errorf("the refusal's limit is %v, want 100", body["limit"])
	}
	if n, m := groupHolds(), dmWithCy(); n != 0 || m != 0 {
		t.Errorf("a refused forward wrote %d to the group and %d to Cy", n, m)
	}

	// A group and a person in one request, read at once: each written once.
	if code, body := forward([]map[string]any{{"grp_id": benGroup}, {"user_uuid": cy.String()}}); code != http.StatusOK {
		t.Fatalf("forwarding to his group and to Cy: %d %v", code, body)
	}
	if n, m := groupHolds(), dmWithCy(); n != 1 || m != 1 {
		t.Errorf("the group holds %d and the DM with Cy %d, want 1 each", n, m)
	}

	// A group he isn't in beside his own: refused, and his own isn't written.
	anaGroup := "grp-" + uuid.NewString()
	dg.Mutate(t, []map[string]any{{"uid": "_:g", "dgraph.type": "Dm", "dm_grouping_id": anaGroup, "dm_participants": []map[string]any{{"uid": got["ana"]}, {"uid": got["cy"]}}}})
	if code, _ := forward([]map[string]any{{"grp_id": benGroup}, {"grp_id": anaGroup}}); code != http.StatusForbidden {
		t.Errorf("forwarding into a group he isn't in: %d, want 403", code)
	}
	// Someone with no account: refused.
	if code, _ := forward([]map[string]any{{"user_uuid": uuid.NewString()}}); code != http.StatusBadRequest {
		t.Errorf("forwarding to no one: %d, want 400", code)
	}
	if n := groupHolds(); n != 1 {
		t.Errorf("after the refusals the group holds %d, want 1", n)
	}
}
