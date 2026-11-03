//go:build integration

package integration_test

// T2: a doc's or a board's people losing their access or role leave no node or
// edge that says who had it. The change is made only once its audit row is
// written (no record, no action): whom it was taken from, from what to what,
// who took it and when. A grant writes no such row, and a refused change none.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	boardController "github.com/akashc777/OneCamp/controllers/Board"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestEveryDocAndBoardAccessTakenIsAudited(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}

	people := map[string]uuid.UUID{"maya": uuid.New(), "kim": uuid.New(), "lee": uuid.New(), "sam": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	board := uuid.NewString()
	nodes = append(nodes, map[string]any{"uid": "_:b", "dgraph.type": "Board", "board_uuid": board, "board_title": "Launch plan",
		"board_private": true, "board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": "_:maya"},
		"board_editing_users": []map[string]any{{"uid": "_:kim"}}, "board_reading_users": []map[string]any{{"uid": "_:lee"}}})
	uids := dg.Mutate(t, nodes)
	priv := true
	doc, err := docBusiness.CreateDocInTree(ctx, &dgraphStruct.DgraphUser{Uid: uids["maya"], Uuid: people["maya"].String(), UserName: "maya"},
		docBusiness.NewDoc{Title: "Quarterly budget", Private: &priv}, nil)
	if err != nil {
		t.Fatal(err)
	}

	as := func(name string) context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people[name])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", name, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people[name].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", name, err)
		}
		return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	}
	call := func(handler http.HandlerFunc, by string, body map[string]any) int {
		t.Helper()
		raw, _ := json.Marshal(body)
		rctx, cancel := context.WithTimeout(as(by), 30*time.Second)
		defer cancel()
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(rctx))
		return rec.Code
	}
	id := func(name string) string { return people[name].String() }
	count := func(t *testing.T, action string) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE action = $1`, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// check reads the newest row of action: Maya, a person, took it, just now,
	// on the resource under key, from exactly the people in want
	// ("<uuid> <from>><to>").
	check := func(t *testing.T, action, key, resource string, want ...string) {
		t.Helper()
		var actor, kind, meta string
		var recent bool
		if err := env.PG.QueryRow(`SELECT COALESCE(actor_id::text, ''), COALESCE(actor_kind, ''), COALESCE(metadata, '{}'),
			created_at > NOW() - interval '10 minutes' FROM admin_audit_log WHERE action = $1 ORDER BY seq DESC LIMIT 1`, action).
			Scan(&actor, &kind, &meta, &recent); err != nil {
			t.Fatalf("%s row: %v", action, err)
		}
		var m struct {
			Doc    string              `json:"doc_uuid"`
			Board  string              `json:"board_uuid"`
			People []map[string]string `json:"people"`
		}
		if err := json.Unmarshal([]byte(meta), &m); err != nil {
			t.Fatalf("%s metadata %s: %v", action, meta, err)
		}
		got := map[string]string{"doc": m.Doc, "board": m.Board}[key]
		var shifts []string
		for _, p := range m.People {
			shifts = append(shifts, p["user_uuid"]+" "+p["from"]+">"+p["to"])
		}
		sort.Strings(shifts)
		sort.Strings(want)
		if actor != id("maya") || kind != "human" || !recent || got != resource || !equalStrings(shifts, want) {
			t.Fatalf("%s row: actor %s (%s), recent %v, %s %s, people %v; want maya (human), now, %s, %v",
				action, actor, kind, recent, key, got, shifts, resource, want)
		}
	}

	t.Run("a doc's grant writes no row", func(t *testing.T) {
		maya := accessBusiness.Subject{UserUID: uids["maya"], UserUUID: id("maya")}
		if _, err := accessBusiness.Apply(as("maya"), maya, accessBusiness.Change{
			Resource: accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: doc.Uuid},
			Add: []accessBusiness.Grant{{Kind: accessBusiness.PrincipalUser, UUID: id("kim"), Level: accessBusiness.LevelEdit},
				{Kind: accessBusiness.PrincipalUser, UUID: id("lee"), Level: accessBusiness.LevelView}},
		}); err != nil {
			t.Fatalf("Maya sharing the doc: %v", err)
		}
		if n := count(t, "doc.access.remove"); n != 0 {
			t.Fatalf("%d doc.access.remove rows after a grant, want none", n)
		}
	})

	t.Run("the share dialog lowering Kim and taking Lee off is recorded first", func(t *testing.T) {
		if code := call(docController.UpdateDocSharing, "maya", map[string]any{"doc_uuid": doc.Uuid,
			"add_viewers": []string{id("kim")}, "remove_editors": []string{id("kim")}, "remove_viewers": []string{id("lee")}}); code != http.StatusOK {
			t.Fatalf("Maya's change answered %d, want 200", code)
		}
		check(t, "doc.access.remove", "doc", doc.Uuid, id("kim")+" edit>view", id("lee")+" view>")
	})

	t.Run("a refused doc change writes no row", func(t *testing.T) {
		if code := call(docController.UpdateDocSharing, "kim", map[string]any{"doc_uuid": doc.Uuid,
			"remove_viewers": []string{id("maya")}}); code == http.StatusOK {
			t.Fatal("Kim, a viewer, changed the doc's sharing")
		}
		if n := count(t, "doc.access.remove"); n != 1 {
			t.Fatalf("%d doc.access.remove rows after a refusal, want 1", n)
		}
	})

	t.Run("the board's owner lowering Kim and taking Lee off is recorded first", func(t *testing.T) {
		if code := call(boardController.UpdateBoardPermissions, "maya", map[string]any{"board_uuid": board,
			"add_viewers": []string{id("kim")}, "remove_viewers": []string{id("lee")}}); code != http.StatusOK {
			t.Fatalf("Maya's change answered %d, want 200", code)
		}
		check(t, "board.access.remove", "board", board, id("kim")+" edit>view", id("lee")+" view>")
	})

	t.Run("a board's grant and a refused change write no row", func(t *testing.T) {
		if code := call(boardController.UpdateBoardPermissions, "maya", map[string]any{"board_uuid": board,
			"add_commenters": []string{id("sam")}}); code != http.StatusOK {
			t.Fatalf("Maya making Sam a commenter answered %d, want 200", code)
		}
		if code := call(boardController.UpdateBoardPermissions, "kim", map[string]any{"board_uuid": board,
			"remove_commenters": []string{id("sam")}}); code == http.StatusOK {
			t.Fatal("Kim, a viewer, changed the board's roles")
		}
		if n := count(t, "board.access.remove"); n != 1 {
			t.Fatalf("%d board.access.remove rows, want only the first change's", n)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
