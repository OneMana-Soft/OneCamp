//go:build integration
// +build integration

package integration_test

// Sub-pages in step with the tree (docs C7a), through the handlers, against
// real Postgres 12 and Dgraph, with the collaboration service's sub-page ops
// recorded: a page made under a page takes its block there, a move takes the
// block out of the old parent's body and onto the end of the new one's, a
// restore shows the page in its parent's body again, and a body the person
// may not edit is left as it is.
//
// Run: go test -tags=integration ./tests/integration/ -run TestASubpagesBlockFollowsItThroughTheTree -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestASubpagesBlockFollowsItThroughTheTree(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[],"result":"updated","_id":"x"}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	people := map[string]uuid.UUID{"ana": uuid.New(), "ben": uuid.New()}
	byUUID := map[string]string{}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, is_admin, created_at, updated_at) VALUES ($1, $2, $3, $4, false, NOW(), NOW())`,
			id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
		byUUID[id.String()] = name
	}
	uids := dg.Mutate(t, nodes)
	call := func(who, target string, handler http.HandlerFunc, body any) answer {
		t.Helper()
		raw, _ := json.Marshal(body)
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserPostgresInfo.EmailID = people[who].String() + "@example.test"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[who], Uuid: people[who].String(), UserName: who}
		req := httptest.NewRequest("POST", target, bytes.NewReader(raw)).WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, u))
		rec := httptest.NewRecorder()
		handler(rec, req)
		out := answer{code: rec.Code}
		_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
		return out
	}

	// The collaboration service's sub-page ops, noted as they run in the
	// background, and read as "remove|place <doc> <page> <by>" once settled.
	title := map[string]string{}
	var mu sync.Mutex
	var edits [][4]string
	note := func(op, doc, page string, o collab.Options) {
		mu.Lock()
		defer mu.Unlock()
		edits = append(edits, [4]string{op, doc, page, o.By.UserUUID})
	}
	remove, place := collab.RemoveSubpage, collab.PlaceSubpage
	collab.RemoveSubpage = func(_ context.Context, doc, page string, o collab.Options) (collab.Result, error) {
		note("remove", doc, page, o)
		return collab.Result{Removed: 1, Persisted: true}, nil
	}
	collab.PlaceSubpage = func(_ context.Context, doc, page string, o collab.Options) (collab.Result, error) {
		note("place", doc, page, o)
		return collab.Result{Blocks: 1, Persisted: true}, nil
	}
	t.Cleanup(func() { collab.RemoveSubpage, collab.PlaceSubpage = remove, place })
	expect := func(step string, want ...string) {
		t.Helper()
		if left := helpers.SettleBackground(20 * time.Second); len(left) != 0 {
			t.Fatalf("%s: still running %v", step, left)
		}
		mu.Lock()
		var got []string
		for _, e := range edits {
			got = append(got, e[0]+" "+title[e[1]]+" "+title[e[2]]+" "+byUUID[e[3]])
		}
		edits = nil
		mu.Unlock()
		if !slices.Equal(got, want) {
			t.Errorf("%s: wrote %q, want %q", step, got, want)
		}
	}

	create := func(name, parent string, placed bool) string {
		t.Helper()
		a := call("ana", "/doc/createDoc", docController.CreateDocInTree, map[string]any{"doc_title": name, "parent_uuid": parent, "doc_private": false, "placed": placed})
		if a.code != 200 {
			t.Fatalf("making %q under %q: %d %v", name, title[parent], a.code, a.body)
		}
		id := str(a.data()["doc_uuid"])
		title[id] = name
		return id
	}
	move := func(who, id, parent, after string) {
		t.Helper()
		body := map[string]any{"doc_uuid": id, "parent_uuid": parent, "after_uuid": after, "place": "last"}
		if parent == "" {
			body["space"] = "workspace"
		}
		if a := call(who, "/doc/move", docController.MoveDoc, body); a.code != 200 {
			t.Fatalf("%s moving %q under %q: %d %v", who, title[id], title[parent], a.code, a.body)
		}
	}

	plans := create("Plans", "", false)
	quarter := create("Quarter", "", false)
	expect("two pages at the top")
	roadmap := create("Roadmap", plans, false)
	expect("a page made under a page", "place Plans Roadmap ana")
	create("Retro", plans, true)
	expect(`a page made with "/page", which places its own block`)

	move("ana", roadmap, quarter, "")
	expect("a page moved to another page", "remove Plans Roadmap ana", "place Quarter Roadmap ana")
	launch := create("Launch", quarter, false)
	expect("a second page in Quarter", "place Quarter Launch ana")
	move("ana", roadmap, quarter, launch)
	expect("a page moved among its own siblings")
	move("ana", roadmap, "", "")
	expect("a page moved to the top", "remove Quarter Roadmap ana")
	move("ana", roadmap, plans, "")
	expect("a page moved from the top", "place Plans Roadmap ana")

	// Ben may edit Roadmap and Quarter, and only read Plans: his move takes
	// the page, and leaves Plans' body as it is, the block a link to it.
	for id, pred := range map[string]string{plans: "doc_reading_users", quarter: "doc_editing_users", roadmap: "doc_editing_users"} {
		dg.Mutate(t, map[string]any{"uid": uidOf(t, dg, id), "doc_private": true, pred: []map[string]any{{"uid": uids["ben"]}}})
	}
	move("ben", roadmap, quarter, "")
	expect("a page moved out of a page whose body the mover may not edit", "place Quarter Roadmap ben")

	if a := call("ana", "/doc/delete", docController.DeleteDocTree, map[string]any{"doc_uuid": roadmap}); a.code != 200 {
		t.Fatalf("deleting Roadmap: %d %v", a.code, a.body)
	}
	expect("a page put in the trash, its block left to read Deleted page")
	if a := call("ana", "/doc/restore", docController.RestoreDocFromTrash, map[string]any{"doc_uuid": roadmap}); a.code != 200 {
		t.Fatalf("restoring Roadmap: %d %v", a.code, a.body)
	}
	expect("a page back from the trash", "place Quarter Roadmap ana")

	// Its block deleted from its parent's body (docs C7a, part 3), a page goes
	// to the trash only while it is that page's child, and the editor's Undo
	// puts the block back itself, so the restore places none.
	plan := create("Plan", plans, false)
	expect("another page in Plans", "place Plans Plan ana")
	if a := call("ana", "/doc/delete", docController.DeleteDocTree, map[string]any{"doc_uuid": plan}); a.code != 200 {
		t.Fatalf("deleting Plan: %d %v", a.code, a.body)
	}
	if a := call("ana", "/doc/restore", docController.RestoreDocFromTrash, map[string]any{"doc_uuid": plan, "placed": true}); a.code != 200 {
		t.Fatalf("restoring Plan: %d %v", a.code, a.body)
	}
	expect("a page back from the trash whose block the editor put back")
	if a := call("ana", "/doc/delete", docController.DeleteDocTree, map[string]any{"doc_uuid": plan, "parent_uuid": quarter}); a.code != http.StatusConflict || str(a.body["code"]) != "not_under" {
		t.Errorf("deleting Plan as Quarter's child: %d %v, want 409 not_under, and Plan left as it is", a.code, a.body)
	}
	if a := call("ana", "/doc/delete", docController.DeleteDocTree, map[string]any{"doc_uuid": plan, "parent_uuid": plans}); a.code != 200 {
		t.Errorf("deleting Plan as Plans' child: %d %v", a.code, a.body)
	}
	expect("a page whose block was deleted from its parent")
}
