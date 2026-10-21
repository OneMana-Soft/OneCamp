//go:build integration
// +build integration

package integration_test

// A forward names where it goes by graph uids (channel_dgraph_uid,
// grp_dgraph_uid, user_dgraph_uid), and every one is checked or replaced
// before anything is written: a channel's uid must be a channel the sender is
// in (domain/Channel GetChannelListWithMemberFlag: has(ch_uuid) and
// ch_members), and a group's or a person's node is read from its grouping id
// or uuid, whatever uid the request carried. This is the evidence behind
// those three fields' entries in helpers/testdata/uid-trust.txt. Against
// real Postgres, Redis and Dgraph.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAForwardWritesOnlyWhereItChecked -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAForwardWritesOnlyWhereItChecked(t *testing.T) {
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
	// Ana has a private channel and a group with Cy; Ben is in neither, and
	// is in #open with Ana.
	ana, ben, cy := uuid.New(), uuid.New(), uuid.New()
	private, open := uuid.New(), uuid.New()
	anaGroup, benGroup := "grp-"+uuid.NewString(), "grp-"+uuid.NewString()
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'ana-only', true), ($2, 'open', false)`, private, open)
	for name, id := range map[string]uuid.UUID{"ana": ana, "ben": ben, "cy": cy} {
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
	}
	live := "0001-01-01T00:00:00Z"
	got := dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": ana.String(), "user_name": "ana"},
		{"uid": "_:ben", "dgraph.type": "User", "user_uuid": ben.String(), "user_name": "ben"},
		{"uid": "_:cy", "dgraph.type": "User", "user_uuid": cy.String(), "user_name": "cy"},
		{"uid": "_:private", "dgraph.type": "Channel", "ch_uuid": private.String(), "ch_name": "ana-only", "ch_private": true, "ch_deleted_at": live,
			"ch_members": []map[string]any{{"uid": "_:ana"}}},
		{"uid": "_:open", "dgraph.type": "Channel", "ch_uuid": open.String(), "ch_name": "open", "ch_private": false, "ch_deleted_at": live,
			"ch_members": []map[string]any{{"uid": "_:ana"}, {"uid": "_:ben"}}},
		{"uid": "_:anagroup", "dgraph.type": "Dm", "dm_grouping_id": anaGroup, "dm_participants": []map[string]any{{"uid": "_:ana"}, {"uid": "_:cy"}}},
		{"uid": "_:bengroup", "dgraph.type": "Dm", "dm_grouping_id": benGroup, "dm_participants": []map[string]any{{"uid": "_:ben"}, {"uid": "_:cy"}}},
		{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": uuid.NewString(), "doc_title": "Salaries", "doc_private": true, "doc_deleted_at": live,
			"doc_created_by": map[string]any{"uid": "_:ana"}, "doc_created_at": time.Now().UTC().Format(time.RFC3339)},
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
	forward := func(to map[string]any) int {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"fwd_text": "<p>fyi</p>", "fwd_list": []map[string]any{to}})
		rec := httptest.NewRecorder()
		userController.FwdUserMessage(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(asBen))
		return rec.Code
	}
	count := func(q string) int {
		t.Helper()
		var out map[string][]map[string]int
		if err := json.Unmarshal(dg.Query(t, q), &out); err != nil || len(out["q"]) != 1 {
			t.Fatalf("%s: %v %v", q, out, err)
		}
		return out["q"][0]["n"]
	}
	untouched := func(when string) {
		t.Helper()
		if n := count(fmt.Sprintf(`{ q(func: uid(%s)) { n: count(ch_posts) } }`, got["private"])); n != 0 {
			t.Errorf("%s: Ana's private channel holds %d posts", when, n)
		}
		if n := count(fmt.Sprintf(`{ q(func: uid(%s)) { n: count(dm_chats) } }`, got["anagroup"])); n != 0 {
			t.Errorf("%s: Ana's group holds %d messages", when, n)
		}
		var doc map[string][]struct {
			Types []string `json:"dgraph.type"`
			Posts []any    `json:"ch_posts"`
			Chats []any    `json:"dm_chats"`
		}
		_ = json.Unmarshal(dg.Query(t, fmt.Sprintf(`{ q(func: uid(%s)) { dgraph.type ch_posts { uid } dm_chats { uid } } }`, got["doc"])), &doc)
		if d := doc["q"]; len(d) != 1 || len(d[0].Types) != 1 || d[0].Types[0] != "Doc" || len(d[0].Posts) != 0 || len(d[0].Chats) != 0 {
			t.Errorf("%s: Ana's doc changed: %+v", when, d)
		}
	}

	// A channel he isn't in, and a node that isn't a channel: refused.
	if code := forward(map[string]any{"channel_uuid": open.String(), "channel_dgraph_uid": got["private"]}); code != http.StatusForbidden {
		t.Errorf("forwarding into a channel he isn't in, by its uid: %d, want 403", code)
	}
	if code := forward(map[string]any{"channel_dgraph_uid": got["doc"]}); code != http.StatusForbidden {
		t.Errorf("forwarding into a doc's uid as a channel: %d, want 403", code)
	}
	untouched("after the refused channels")

	// His own group, carrying Ana's group's uid and the doc's as a person's:
	// the message goes to his group, read by its grouping id.
	if code := forward(map[string]any{"grp_id": benGroup, "grp_dgraph_uid": got["anagroup"], "user_dgraph_uid": got["doc"]}); code != http.StatusOK {
		t.Fatalf("forwarding into his own group: %d", code)
	}
	if n := count(fmt.Sprintf(`{ q(func: uid(%s)) { n: count(dm_chats) } }`, got["bengroup"])); n != 1 {
		t.Errorf("his group holds %d messages, want 1", n)
	}
	// Ana's group by its grouping id: he isn't in it, refused.
	if code := forward(map[string]any{"grp_id": anaGroup, "grp_dgraph_uid": got["anagroup"]}); code != http.StatusForbidden {
		t.Errorf("forwarding into a group he isn't in: %d, want 403", code)
	}
	// A person, carrying the doc's uid as theirs and as a group's: the
	// message goes to the DM with Cy, read by Cy's uuid.
	if code := forward(map[string]any{"user_uuid": cy.String(), "user_dgraph_uid": got["doc"], "grp_dgraph_uid": got["doc"]}); code != http.StatusOK {
		t.Fatalf("forwarding to Cy: %d", code)
	}
	if n := count(fmt.Sprintf(`{ q(func: eq(dm_grouping_id, %q)) { n: count(dm_chats) } }`, helpers.GetGroupingId(ben.String(), cy.String()))); n != 1 {
		t.Errorf("Ben's DM with Cy holds %d messages, want 1", n)
	}
	untouched("after every forward")
}
