//go:build integration
// +build integration

package integration_test

// A3's acceptance, through the handlers, against real Postgres 12 and Dgraph
// (search answered by a stub): the page tree, its loop refusal, redaction,
// the lists and their counts, the trash as a unit and who it answers,
// duplicate, stars, and migration 206 down and up again.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDocTreeAcceptance -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

type answer struct {
	code int
	body map[string]any
}

func (a answer) data() map[string]any {
	d, _ := a.body["data"].(map[string]any)
	return d
}

func list(v any) []any { l, _ := v.([]any); return l }

func str(v any) string { s, _ := v.(string); return s }

func num(v any) int { f, _ := v.(float64); return int(f) }

func TestDocTreeAcceptance(t *testing.T) {
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

	// Dee is a workspace admin.
	people := map[string]uuid.UUID{"ana": uuid.New(), "ben": uuid.New(), "cy": uuid.New(), "dee": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, is_admin, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, NOW(), NOW())`,
			id, id.String()+"@example.test", name, name, name == "dee"); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	uids := dg.Mutate(t, nodes)
	as := func(name string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name]
		u.UserPostgresInfo.EmailID = people[name].String() + "@example.test"
		u.UserPostgresInfo.IsAdmin = name == "dee"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: people[name].String(), UserName: name}
		return u
	}
	call := func(who string, method, target string, handler http.HandlerFunc, body any, params map[string]string) answer {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, target, bytes.NewReader(raw))
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		c := context.WithValue(ctx, helpers.UserInfoContextKey, as(who))
		req = req.WithContext(context.WithValue(c, chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		handler(rec, req)
		out := answer{code: rec.Code}
		_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
		return out
	}
	create := func(who, title, parent string, private bool) string {
		t.Helper()
		a := call(who, "POST", "/doc/createDoc", docController.CreateDocInTree, map[string]any{"doc_title": title, "parent_uuid": parent, "doc_private": private}, nil)
		if a.code != 200 {
			t.Fatalf("%s making %q under %q: %d %v", who, title, parent, a.code, a.body)
		}
		return str(a.data()["doc_uuid"])
	}
	children := func(who, parent, space string, limit int) answer {
		q := "/doc/children?parent=" + parent + "&space=" + space
		if limit > 0 {
			q += fmt.Sprintf("&limit=%d", limit)
		}
		return call(who, "GET", q, docController.GetDocChildren, nil, nil)
	}
	titles := func(a answer, key string) []string {
		var out []string
		for _, n := range list(a.data()[key]) {
			m := n.(map[string]any)
			t := str(m["title"])
			if t == "" {
				t = str(m["doc_title"])
			}
			out = append(out, t)
		}
		return out
	}

	// A > B > C, all Ana's, public.
	a := create("ana", "A", "", false)
	b := create("ana", "B", a, false)
	c := create("ana", "C", b, false)

	t.Run("a move that would close a loop answers 409", func(t *testing.T) {
		got := call("ana", "POST", "/doc/move", docController.MoveDoc, map[string]any{"doc_uuid": a, "parent_uuid": c}, nil)
		if got.code != http.StatusConflict || str(got.body["code"]) != "cycle" {
			t.Fatalf("A under C: %d %v", got.code, got.body)
		}
		if got := call("ana", "POST", "/doc/move", docController.MoveDoc, map[string]any{"doc_uuid": a, "parent_uuid": a}, nil); got.code != http.StatusConflict {
			t.Fatalf("A under itself: %d", got.code)
		}
	})

	t.Run("children come in position order, and the path is right", func(t *testing.T) {
		b2 := create("ana", "B2", a, false)
		call("ana", "POST", "/doc/move", docController.MoveDoc, map[string]any{"doc_uuid": b2, "parent_uuid": a, "place": "first"}, nil)
		got := children("ana", a, "", 0)
		if got.code != 200 || fmt.Sprint(titles(got, "nodes")) != "[B2 B]" {
			t.Fatalf("A's children: %d %v", got.code, titles(got, "nodes"))
		}
		for _, n := range list(got.data()["nodes"]) {
			m := n.(map[string]any)
			if str(m["title"]) == "B" && m["has_children"] != true {
				t.Fatalf("B says it has no children")
			}
		}
		path := call("ana", "GET", "/doc/ancestors/"+c, docController.GetDocAncestors, nil, map[string]string{"doc_uuid": c})
		if fmt.Sprint(titles(path, "ancestors")) != "[A B]" {
			t.Fatalf("C's path: %v", path.body)
		}
		call("ana", "POST", "/doc/delete", docController.DeleteDocTree, map[string]any{"doc_uuid": b2}, nil)
	})

	t.Run("pages someone can't open are counted, never named", func(t *testing.T) {
		// A subpage takes its parent's privacy; these two are then made private.
		for _, title := range []string{"Secret one", "Secret two"} {
			id := create("ana", title, a, true)
			dg.Mutate(t, map[string]any{"uid": uidOf(t, dg, id), "doc_private": true})
		}
		got := children("ben", a, "", 0)
		if got.code != 200 || num(got.data()["hidden"]) != 2 || fmt.Sprint(titles(got, "nodes")) != "[B]" {
			t.Fatalf("Ben's view of A: %d %v", got.code, got.body)
		}
		raw, _ := json.Marshal(got.body)
		if bytes.Contains(raw, []byte("Secret")) {
			t.Fatalf("a title Ben can't open was sent: %s", raw)
		}
		// A private page above a shared one is a gap in the path, not a name.
		p := create("ana", "Private plans", "", true)
		q4 := create("ana", "Q4", p, true)
		dg.Mutate(t, map[string]any{"uid": uidOf(t, dg, q4), "doc_reading_users": []map[string]any{{"uid": uids["ben"]}}})
		path := call("ben", "GET", "/doc/ancestors/"+q4, docController.GetDocAncestors, nil, map[string]string{"doc_uuid": q4})
		anc := list(path.data()["ancestors"])
		if path.code != 200 || len(anc) != 1 || anc[0].(map[string]any)["redacted"] != true || str(anc[0].(map[string]any)["title"]) != "" || str(anc[0].(map[string]any)["uuid"]) != "" {
			t.Fatalf("Ben's path to Q4: %d %v", path.code, path.body)
		}
		top := children("ben", "", "shared", 0)
		if fmt.Sprint(titles(top, "nodes")) != "[Q4]" {
			t.Fatalf("Ben's shared top: %v", top.body)
		}
	})

	t.Run("lists hold the right docs, and counts equal what the pages hold", func(t *testing.T) {
		x := create("cy", "Cy's private note", "", true)
		dg.Mutate(t, map[string]any{"uid": uidOf(t, dg, x), "doc_editing_users": []map[string]any{{"uid": uids["ben"]}}})
		has := func(who, scope, title string) bool {
			got := call(who, "GET", "/doc/list?scope="+scope+"&limit=100", docController.GetDocList, nil, nil)
			if got.code != 200 {
				t.Fatalf("%s %s: %d %v", who, scope, got.code, got.body)
			}
			for _, tt := range titles(got, "docs") {
				if tt == title {
					return true
				}
			}
			return false
		}
		if !has("ben", "shared", "Cy's private note") || !has("cy", "mine", "Cy's private note") ||
			has("ben", "workspace", "Cy's private note") || has("cy", "workspace", "Cy's private note") || has("ana", "shared", "Cy's private note") {
			t.Fatal("a private doc shared with Ben: Ben's Shared and Cy's Mine, nobody's Workspace")
		}
		for _, scope := range []string{"mine", "workspace", "shared"} {
			total, seen, cursor := -1, 0, ""
			for i := 0; i < 50; i++ {
				got := call("ana", "GET", "/doc/list?scope="+scope+"&limit=2&cursor="+cursor, docController.GetDocList, nil, nil)
				if total < 0 {
					total = num(got.data()["count"])
				} else if num(got.data()["count"]) != total {
					t.Fatalf("%s: the count moved between pages", scope)
				}
				seen += len(list(got.data()["docs"]))
				cursor = str(got.data()["next_cursor"])
				if cursor == "" {
					break
				}
			}
			if seen != total {
				t.Fatalf("%s counts %d and its pages hold %d", scope, total, seen)
			}
		}
	})

	t.Run("a subtree deleted together comes back together; an old delete comes back alone", func(t *testing.T) {
		del := call("ana", "POST", "/doc/delete", docController.DeleteDocTree, map[string]any{"doc_uuid": b}, nil)
		if del.code != 200 || num(del.data()["count"]) != 2 {
			t.Fatalf("deleting B: %d %v", del.code, del.body)
		}
		for _, title := range titles(children("ana", a, "", 0), "nodes") {
			if title == "B" {
				t.Fatal("A still lists B")
			}
		}
		trash := call("ana", "GET", "/doc/trash", docController.GetDocTrash, nil, nil)
		var bItem map[string]any
		for _, it := range list(trash.data()["items"]) {
			m := it.(map[string]any)
			if str(m["doc_title"]) == "C" {
				t.Fatalf("C, deleted with B, is its own trash entry")
			}
			if str(m["doc_title"]) == "B" {
				bItem = m
			}
		}
		if bItem == nil || num(bItem["subpages"]) != 1 {
			t.Fatalf("the trash: %v", trash.body)
		}
		// Restoring C restores the whole delete.
		if got := call("ana", "POST", "/doc/restore", docController.RestoreDocFromTrash, map[string]any{"doc_uuid": c}, nil); got.code != 200 || num(got.data()["restored"]) != 2 {
			t.Fatalf("restoring: %d %v", got.code, got.body)
		}
		if got := children("ana", b, "", 0); fmt.Sprint(titles(got, "nodes")) != "[C]" {
			t.Fatalf("C isn't back under B: %v", got.body)
		}
		// A doc deleted before batches existed comes back on its own, at the
		// top when its parent is gone.
		old := create("ana", "Old child", c, false)
		other := create("ana", "Sibling", c, false)
		dg.Mutate(t, map[string]any{"uid": uidOf(t, dg, old), "doc_deleted_at": "2026-10-01T00:00:00Z"})
		dg.Mutate(t, map[string]any{"uid": uidOf(t, dg, c), "doc_deleted_at": "2026-10-01T00:00:00Z"})
		got := call("ana", "POST", "/doc/restore", docController.RestoreDocFromTrash, map[string]any{"doc_uuid": old}, nil)
		if got.code != 200 || num(got.data()["restored"]) != 1 || str(got.data()["parent_uuid"]) != "" {
			t.Fatalf("restoring an old delete: %d %v", got.code, got.body)
		}
		_ = other
		if got := call("cy", "POST", "/doc/purge", docController.PurgeDocForGood, map[string]any{"doc_uuid": c}, nil); got.code != http.StatusForbidden && got.code != http.StatusNotFound {
			t.Fatalf("Cy purging Ana's doc: %d", got.code)
		}
	})

	t.Run("restore and delete for good tell nobody a page they never had is there", func(t *testing.T) {
		// Ana's private page, which Ben may read and Cy never had.
		p := create("ana", "Salaries", "", true)
		dg.Mutate(t, map[string]any{"uid": uidOf(t, dg, p), "doc_private": true, "doc_reading_users": []map[string]any{{"uid": uids["ben"]}}})
		ask := func(who, what, id string) answer {
			t.Helper()
			if what == "restore" {
				return call(who, "POST", "/doc/restore", docController.RestoreDocFromTrash, map[string]any{"doc_uuid": id}, nil)
			}
			return call(who, "POST", "/doc/purge", docController.PurgeDocForGood, map[string]any{"doc_uuid": id}, nil)
		}
		missing := ask("cy", "restore", uuid.NewString())
		if missing.code != http.StatusNotFound {
			t.Fatalf("restoring a page that isn't there: %d %v", missing.code, missing.body)
		}
		asMissing := func(when string) {
			t.Helper()
			for _, what := range []string{"restore", "purge"} {
				if got := ask("cy", what, p); got.code != missing.code || fmt.Sprint(got.body) != fmt.Sprint(missing.body) {
					t.Errorf("Cy asking to %s a page %s: %d %v, where a page that isn't there answers %d %v", what, when, got.code, got.body, missing.code, missing.body)
				}
			}
		}
		asMissing("that's live")
		if got := ask("ben", "restore", p); got.code != http.StatusConflict || str(got.body["code"]) != "not_in_trash" {
			t.Errorf("Ben, who reads it, restoring it while it's live: %d %v", got.code, got.body)
		}
		if del := call("ana", "POST", "/doc/delete", docController.DeleteDocTree, map[string]any{"doc_uuid": p}, nil); del.code != 200 {
			t.Fatalf("Ana deleting it: %d %v", del.code, del.body)
		}
		asMissing("in the trash")
		for _, what := range []string{"restore", "purge"} {
			if got := ask("ben", what, p); got.code != http.StatusForbidden {
				t.Errorf("Ben, who could only read it, asking to %s it: %d %v", what, got.code, got.body)
			}
		}
		// An admin may bring back what they never had, and its owner may
		// delete it for good, after which it is missing to everyone.
		if got := ask("dee", "restore", p); got.code != 200 || num(got.data()["restored"]) != 1 {
			t.Fatalf("Dee, an admin, restoring it: %d %v", got.code, got.body)
		}
		if del := call("ana", "POST", "/doc/delete", docController.DeleteDocTree, map[string]any{"doc_uuid": p}, nil); del.code != 200 {
			t.Fatalf("Ana deleting it again: %d %v", del.code, del.body)
		}
		if got := ask("ana", "purge", p); got.code != 200 || num(got.data()["purged"]) != 1 {
			t.Fatalf("Ana deleting it for good: %d %v", got.code, got.body)
		}
		asMissing("deleted for good")
		if got := ask("ben", "restore", p); got.code != missing.code {
			t.Errorf("Ben restoring a page deleted for good: %d %v", got.code, got.body)
		}
	})

	t.Run("two moves racing to close a loop: one of them fails", func(t *testing.T) {
		for round := 0; round < 4; round++ {
			x, y := create("ana", "X", "", false), create("ana", "Y", "", false)
			var wg sync.WaitGroup
			codes := make([]int, 2)
			for i, m := range [][2]string{{x, y}, {y, x}} {
				wg.Add(1)
				go func(i int, m [2]string) {
					defer wg.Done()
					codes[i] = call("ana", "POST", "/doc/move", docController.MoveDoc, map[string]any{"doc_uuid": m[0], "parent_uuid": m[1]}, nil).code
				}(i, m)
			}
			wg.Wait()
			sort.Ints(codes)
			if codes[0] != 200 || codes[1] != http.StatusConflict {
				t.Fatalf("round %d: %v", round, codes)
			}
		}
	})

	t.Run("duplicate copies a page with its subpages, as the person's", func(t *testing.T) {
		top := create("ben", "Handbook", "", false)
		create("ben", "Chapter", top, false)
		got := call("ben", "POST", "/doc/duplicate", docController.DuplicateDoc, map[string]any{"doc_uuid": top, "with_children": true}, nil)
		if got.code != 200 || num(got.data()["count"]) != 2 {
			t.Fatalf("duplicate: %d %v", got.code, got.body)
		}
		kids := children("ben", str(got.data()["doc_uuid"]), "", 0)
		if fmt.Sprint(titles(kids, "nodes")) != "[Chapter]" {
			t.Fatalf("the copy's children: %v", kids.body)
		}
	})

	t.Run("a star, in its place, gone from the list when the doc can't be opened", func(t *testing.T) {
		mine := create("cy", "Starred", "", true)
		if got := call("cy", "POST", "/places", docController.AddPlace, map[string]any{"kind": "star", "resource_kind": "doc", "resource_uuid": mine}, nil); got.code != 200 {
			t.Fatalf("star: %d %v", got.code, got.body)
		}
		if got := call("ben", "POST", "/places", docController.AddPlace, map[string]any{"kind": "star", "resource_kind": "doc", "resource_uuid": mine}, nil); got.code != http.StatusNotFound {
			t.Fatalf("Ben starring Cy's private doc: %d", got.code)
		}
		got := call("cy", "GET", "/places?kind=star", docController.GetPlaces, nil, nil)
		if pl := list(got.data()["places"]); len(pl) != 1 || str(pl[0].(map[string]any)["title"]) != "Starred" {
			t.Fatalf("Cy's stars: %v", got.body)
		}
		if got := call("cy", "GET", "/doc/list?scope=starred", docController.GetDocList, nil, nil); num(got.data()["count"]) != 1 {
			t.Fatalf("Cy's Starred list: %v", got.body)
		}
		call("cy", "DELETE", "/places?kind=star&resource_uuid="+mine, docController.RemovePlace, nil, nil)
		if got := call("cy", "GET", "/places?kind=star", docController.GetPlaces, nil, nil); len(list(got.data()["places"])) != 0 {
			t.Fatalf("unstarred: %v", got.body)
		}
	})

	t.Run("migration 206 goes down and up again", func(t *testing.T) {
		_, file, _, _ := runtime.Caller(0)
		m, err := migrate.New("file://"+filepath.Join(filepath.Dir(file), "..", "..", "migrations"), env.DSN)
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		if v, _, _ := m.Version(); v < 206 {
			t.Fatalf("at %d", v)
		}
		var exists bool
		for v, _, _ := m.Version(); v >= 206; v, _, _ = m.Version() {
			if err := m.Steps(-1); err != nil {
				t.Fatal(err)
			}
		}
		_ = env.PG.QueryRow(`SELECT to_regclass('user_places') IS NOT NULL`).Scan(&exists)
		if exists {
			t.Fatal("user_places is still there after down")
		}
		if err := m.Up(); err != nil {
			t.Fatal(err)
		}
		_ = env.PG.QueryRow(`SELECT to_regclass('user_places') IS NOT NULL`).Scan(&exists)
		if !exists {
			t.Fatal("user_places didn't come back")
		}
	})
}

func uidOf(t *testing.T, dg *integration.DgraphEnv, docUUID string) string {
	t.Helper()
	var out struct {
		Q []struct{ UID string } `json:"q"`
	}
	_ = json.Unmarshal(dg.Query(t, `{ q(func: eq(doc_uuid, "`+docUUID+`")) { uid } }`), &out)
	if len(out.Q) != 1 {
		t.Fatalf("no doc %s", docUUID)
	}
	return out.Q[0].UID
}
