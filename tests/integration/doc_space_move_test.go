//go:build integration
// +build integration

package integration_test

// The Wave A audit's two privacy blockers and the Restricted toggle, through
// the handlers, against real Postgres 12, Dgraph and Redis (search answered by
// a stub, the live service by a recorder):
//   - a move into another space is a change to the page's sharing: Bob, who
//     may edit Alice's private page, can't move it into project X; nor can he
//     with full access to it while a subpage under it isn't his to share;
//     nobody puts a page into a space they aren't in, by moving it there or
//     making it there, but a workspace admin, and a move under a page they
//     can't open never says what space it is in;
//     an allowed move counts the space's people first, tells the owner, and
//     closes the live sockets of the people a move out of the space takes it
//     from;
//   - Bob duplicating a page he may only view gets a copy that is his alone,
//     not one under its parent open to every member;
//   - making a doc Restricted through /doc/updateDoc closes its sockets, and
//     is full access's, as A5's sharing panel is.
//
// Each fails on the code before the fix.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDocSpaceMoveAndCopyPrivacy -v

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

	"github.com/go-chi/chi/v5"
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

// socketLog is the live service, recording which docs' sockets were asked
// again and for whom (nil: everyone's). Seeds answer that the service is
// away, so a copy's body is written directly.
type socketLog struct {
	collab.Writer
	mu    sync.Mutex
	asked map[string][][]string
}

func (l *socketLog) Reauth(_ context.Context, doc string, users []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked[doc] = append(l.asked[doc], append([]string(nil), users...))
	return nil
}

func (l *socketLog) Seed(context.Context, string, string, collab.Options) (collab.Result, error) {
	return collab.Result{}, collab.ErrUnavailable
}

// closedFor waits for a doc's sockets to be asked again, everyone's or
// user's among them.
func (l *socketLog) closedFor(doc, user string, wait time.Duration) bool {
	for end := time.Now().Add(wait); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		l.mu.Lock()
		for _, users := range l.asked[doc] {
			if users == nil || slices.Contains(users, user) {
				l.mu.Unlock()
				return true
			}
		}
		l.mu.Unlock()
	}
	return false
}

func TestDocSpaceMoveAndCopyPrivacy(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
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
	sockets := &socketLog{asked: map[string][][]string{}}
	savedLive := collab.Live
	collab.Live = sockets
	t.Cleanup(func() { collab.Live = savedLive })

	// Alice owns the pages. Project X has Bob and Cy as members and Dee as an
	// admin; Eve isn't in it; Ada is a workspace admin who isn't in it; Zed
	// is in the workspace and nothing else.
	names := []string{"alice", "bob", "cy", "dee", "eve", "ada", "zed"}
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, n := range names {
		people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			people[n], people[n].String()+"@example.test", n, n); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n})
	}
	if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id) VALUES ($1)`, people["ada"].String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	x := uuid.NewString()
	nodes = append(nodes, map[string]any{"uid": "_:x", "dgraph.type": "Project", "project_uuid": x, "project_name": "Launch",
		"project_deleted_at": "0001-01-01T00:00:00Z",
		"project_members":    []map[string]any{{"uid": "_:bob"}, {"uid": "_:cy"}}, "project_admins": []map[string]any{{"uid": "_:dee"}}})
	uids := dg.Mutate(t, nodes)

	as := func(who string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserPostgresInfo.EmailID = people[who].String() + "@example.test"
		u.UserPostgresInfo.IsAdmin = who == "ada"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[who], Uuid: people[who].String(), UserName: who}
		return u
	}
	call := func(who, method, target string, handler http.HandlerFunc, body any, params map[string]string) answer {
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
	create := func(who, title, parent string, body map[string]any) string {
		t.Helper()
		in := map[string]any{"doc_title": title, "parent_uuid": parent, "doc_private": true}
		for k, v := range body {
			in[k] = v
		}
		a := call(who, "POST", "/doc/createDoc", docController.CreateDocInTree, in, nil)
		if a.code != 200 {
			t.Fatalf("%s making %q: %d %v", who, title, a.code, a.body)
		}
		return str(a.data()["doc_uuid"])
	}
	share := func(doc string, body map[string]any) {
		t.Helper()
		body["doc_uuid"] = doc
		if a := call("alice", "POST", "/doc/access/update", docController.UpdateDocAccess, body, nil); a.code != 200 {
			t.Fatalf("sharing %s: %d %v", doc, a.code, a.body)
		}
	}
	grant := func(who, lvl string) map[string]any {
		return map[string]any{"adds": []any{map[string]any{"kind": "user", "uuid": people[who].String(), "level": lvl}}}
	}
	levelOf := func(who, doc string) string {
		t.Helper()
		a := call(who, "GET", "/doc/access/"+doc, docController.GetDocAccess, nil, map[string]string{"doc_uuid": doc})
		switch a.code {
		case 200:
			return str(a.data()["level"])
		case 404:
			return "none"
		}
		t.Fatalf("%s's access to %s: %d %v", who, doc, a.code, a.body)
		return ""
	}
	move := func(who string, body map[string]any) answer {
		return call(who, "POST", "/doc/move", docController.MoveDoc, body, nil)
	}
	preview := func(who string, body map[string]any) answer {
		return call(who, "POST", "/doc/move/preview", docController.PreviewDocMove, body, nil)
	}
	intoX := "project:" + x
	noticesFor := func(who, kind, doc string) int {
		t.Helper()
		integration.Settle(t)
		var n int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM resource_notices WHERE user_uuid = $1 AND kind = $2 AND resource_uuid = $3`,
			people[who], kind, doc).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Alice's private plan, with a subpage made before she shared the plan
	// with Bob, so Bob can't open it.
	plan := create("alice", "Plan", "", nil)
	sub := create("alice", "Salaries", plan, nil)
	share(plan, grant("bob", "edit"))
	if levelOf("bob", plan) != "edit" || levelOf("bob", sub) != "none" {
		t.Fatalf("bob: %s on the plan, %s on its subpage", levelOf("bob", plan), levelOf("bob", sub))
	}
	launchNotes := create("bob", "Launch notes", "", map[string]any{"space": intoX})

	shut := func(t *testing.T) {
		t.Helper()
		for _, doc := range []string{plan, sub} {
			for _, who := range []string{"cy", "dee", "zed"} {
				if got := levelOf(who, doc); got != "none" {
					t.Errorf("%s can %s %s", who, got, doc)
				}
			}
		}
	}

	t.Run("an editor can't move someone's private page into a space", func(t *testing.T) {
		for _, body := range []map[string]any{
			{"doc_uuid": plan, "space": intoX},
			{"doc_uuid": plan, "parent_uuid": launchNotes},
		} {
			if got := move("bob", body); got.code != http.StatusForbidden || str(got.body["code"]) != "needs_full_access" {
				t.Errorf("bob moving the plan %v: %d %v", body, got.code, got.body)
			}
			if got := preview("bob", body); got.code != http.StatusForbidden {
				t.Errorf("bob previewing %v: %d %v", body, got.code, got.body)
			}
		}
		shut(t)
	})

	t.Run("full access to the page isn't enough while a page under it isn't theirs to share", func(t *testing.T) {
		share(plan, grant("bob", "full"))
		if got := move("bob", map[string]any{"doc_uuid": plan, "space": intoX}); got.code != http.StatusForbidden || str(got.body["code"]) != "needs_full_access" {
			t.Errorf("bob moving the plan over Salaries: %d %v", got.code, got.body)
		}
		shut(t)
	})

	t.Run("only the people in a space put pages into it, and a workspace admin", func(t *testing.T) {
		mine := create("eve", "Eve's notes", "", nil)
		for _, space := range []string{intoX, "project:" + uuid.NewString()} {
			if got := move("eve", map[string]any{"doc_uuid": mine, "space": space}); got.code != http.StatusForbidden || str(got.body["code"]) != "not_in_space" {
				t.Errorf("eve moving her page into %s: %d %v", space, got.code, got.body)
			}
		}
		if levelOf("cy", mine) != "none" {
			t.Error("eve's page reached Launch")
		}
		// A page in Launch is no page to Eve: moving hers under it, or
		// previewing that, answers as for no page, never with its space.
		under := map[string]any{"doc_uuid": mine, "parent_uuid": launchNotes}
		if got := move("eve", under); got.code != http.StatusNotFound {
			t.Errorf("eve moving her page under a page in Launch: %d %v", got.code, got.body)
		}
		if got := preview("eve", under); got.code != http.StatusNotFound {
			t.Errorf("eve previewing her page under a page in Launch: %d %v", got.code, got.body)
		}
		adas := create("ada", "Ada's notes", "", nil)
		if got := move("ada", map[string]any{"doc_uuid": adas, "space": "project:" + uuid.NewString()}); got.code != http.StatusNotFound {
			t.Errorf("an admin moving into a project that isn't there: %d %v", got.code, got.body)
		}
		if got := move("ada", map[string]any{"doc_uuid": adas, "space": intoX}); got.code != 200 || levelOf("cy", adas) != "view" {
			t.Errorf("an admin moving into Launch: %d %v, cy %s", got.code, got.body, levelOf("cy", adas))
		}

		// Making a page at the top of a space takes the same: Eve can't put
		// one in Launch, nor learn whether a project exists; Bob, who is in
		// Launch, made Launch notes there above; an admin may, and hears of a
		// project that isn't there.
		makeIn := func(who, space string) answer {
			return call(who, "POST", "/doc/createDoc", docController.CreateDocInTree,
				map[string]any{"doc_title": who + "'s page", "doc_private": true, "space": space}, nil)
		}
		for _, space := range []string{intoX, "project:" + uuid.NewString()} {
			if got := makeIn("eve", space); got.code != http.StatusForbidden || str(got.body["code"]) != "not_in_space" {
				t.Errorf("eve making a page in %s: %d %v", space, got.code, got.body)
			}
		}
		if got := makeIn("ada", "project:"+uuid.NewString()); got.code != http.StatusNotFound {
			t.Errorf("an admin making a page in a project that isn't there: %d %v", got.code, got.body)
		}
		if got := makeIn("ada", intoX); got.code != 200 || levelOf("cy", str(got.data()["doc_uuid"])) != "view" {
			t.Errorf("an admin making a page in Launch: %d %v", got.code, got.body)
		}
	})

	t.Run("an allowed move counts the space's people, then shares, and tells the owner", func(t *testing.T) {
		share(sub, grant("bob", "full"))
		p := preview("bob", map[string]any{"doc_uuid": plan, "space": intoX})
		sp, _ := p.data()["space"].(map[string]any)
		if p.code != 200 || num(p.data()["gain"]) != 2 || num(p.data()["lose"]) != 0 || num(p.data()["pages"]) != 2 ||
			str(sp["name"]) != "Launch" || num(sp["members"]) != 3 || p.data()["was"] != nil || str(p.data()["summary"]) != "2 people gain access." {
			t.Fatalf("the preview: %d %v", p.code, p.body)
		}
		if got := move("bob", map[string]any{"doc_uuid": plan, "space": intoX}); got.code != 200 {
			t.Fatalf("bob moving the plan into Launch: %d %v", got.code, got.body)
		}
		for _, who := range []string{"cy", "dee"} {
			if levelOf(who, plan) != "view" || levelOf(who, sub) != "view" {
				t.Errorf("%s: %s on the plan, %s on Salaries", who, levelOf(who, plan), levelOf(who, sub))
			}
		}
		if levelOf("zed", plan) != "none" {
			t.Error("the space opened the plan to the workspace")
		}
		if n := noticesFor("alice", "doc.moved", plan); n != 1 {
			t.Errorf("alice was told %d times that bob moved her plan into Launch", n)
		}
	})

	t.Run("a move out of the space closes the sockets of the people it takes the page from", func(t *testing.T) {
		sockets.mu.Lock()
		sockets.asked = map[string][][]string{}
		sockets.mu.Unlock()
		// It lands among Alice's private pages, right after one of them: the
		// top level it goes to, not Launch's.
		diary := create("alice", "Diary", "", nil)
		out := map[string]any{"doc_uuid": plan, "space": "private", "after_uuid": diary}
		p := preview("alice", out)
		was, _ := p.data()["was"].(map[string]any)
		if p.code != 200 || p.data()["space"] != nil || str(was["name"]) != "Launch" || num(p.data()["lose"]) != 2 {
			t.Errorf("the preview of the move out of Launch: %d %v", p.code, p.body)
		}
		if got := move("alice", out); got.code != 200 {
			t.Fatalf("alice moving the plan out of Launch, after her diary: %d %v", got.code, got.body)
		}
		top := call("alice", "GET", "/doc/children?space=private", docController.GetDocChildren, nil, nil)
		var order []string
		for _, n := range list(top.data()["nodes"]) {
			order = append(order, str(n.(map[string]any)["uuid"]))
		}
		if at := slices.Index(order, diary); at < 0 || at+1 >= len(order) || order[at+1] != plan {
			t.Errorf("the plan isn't right after the diary among alice's private pages: %v", order)
		}
		shut(t)
		if !sockets.closedFor(plan, people["cy"].String(), 5*time.Second) || !sockets.closedFor(sub, people["cy"].String(), 5*time.Second) {
			t.Errorf("cy's sockets on the plan and Salaries stay open: %v", sockets.asked)
		}
		if n := noticesFor("alice", "doc.moved", plan); n != 1 {
			t.Errorf("alice was told of her own move (%d notices)", n)
		}
	})

	t.Run("a copy of a page someone may only view is theirs alone", func(t *testing.T) {
		// Handbook is a workspace page every member may edit; Alice's private
		// Pay page sits under it, shared with Bob to view, with a subpage.
		handbook := create("alice", "Handbook", "", map[string]any{"doc_private": false})
		share(handbook, map[string]any{"general": map[string]any{"level": "edit"}})
		pay := create("alice", "Pay", handbook, nil)
		share(pay, map[string]any{"general": map[string]any{"level": "none"}})
		share(pay, grant("bob", "view"))
		bands := create("alice", "Bands", pay, nil)
		if levelOf("zed", pay) != "none" || levelOf("bob", bands) != "view" {
			t.Fatalf("zed %s on Pay, bob %s on Bands", levelOf("zed", pay), levelOf("bob", bands))
		}
		got := call("bob", "POST", "/doc/duplicate", docController.DuplicateDoc, map[string]any{"doc_uuid": pay, "with_children": true}, nil)
		if got.code != 200 || num(got.data()["count"]) != 2 {
			t.Fatalf("bob copying Pay: %d %v", got.code, got.body)
		}
		cp := str(got.data()["doc_uuid"])
		kids := call("bob", "GET", "/doc/children?parent="+cp, docController.GetDocChildren, nil, nil)
		var subCopy string
		for _, n := range list(kids.data()["nodes"]) {
			subCopy = str(n.(map[string]any)["uuid"])
		}
		if subCopy == "" {
			t.Fatalf("the copy's subpage: %v", kids.body)
		}
		for _, doc := range []string{cp, subCopy} {
			for _, who := range []string{"zed", "alice", "cy"} {
				if lvl := levelOf(who, doc); lvl != "none" {
					t.Errorf("%s can %s bob's copy %s", who, lvl, doc)
				}
			}
			if levelOf("bob", doc) != "full" {
				t.Errorf("bob's own copy %s: %s", doc, levelOf("bob", doc))
			}
		}
		path := call("bob", "GET", "/doc/ancestors/"+cp, docController.GetDocAncestors, nil, map[string]string{"doc_uuid": cp})
		if path.code != 200 || len(list(path.data()["ancestors"])) != 0 {
			t.Errorf("bob's copy isn't at the top of his pages: %v", path.body)
		}
	})

	t.Run("an editor's copy still goes beside the page, sharing what its parent shares", func(t *testing.T) {
		guide := create("alice", "Guide", "", map[string]any{"doc_private": false})
		share(guide, map[string]any{"general": map[string]any{"level": "edit"}})
		steps := create("alice", "Steps", guide, nil)
		create("alice", "Checklist", steps, nil)
		secret := create("alice", "Secret step", steps, nil)
		share(secret, map[string]any{"general": map[string]any{"level": "none"}})
		share(secret, grant("bob", "view"))
		got := call("bob", "POST", "/doc/duplicate", docController.DuplicateDoc, map[string]any{"doc_uuid": steps, "with_children": true}, nil)
		if got.code != 200 || num(got.data()["count"]) != 3 {
			t.Fatalf("bob copying Steps: %d %v", got.code, got.body)
		}
		cp := str(got.data()["doc_uuid"])
		if lvl := levelOf("zed", cp); lvl != "edit" {
			t.Errorf("the copy beside Steps is %s to the workspace", lvl)
		}
		kids := call("bob", "GET", "/doc/children?parent="+cp, docController.GetDocChildren, nil, nil)
		copied := map[string]string{} // title -> uuid
		for _, n := range list(kids.data()["nodes"]) {
			copied[str(n.(map[string]any)["title"])] = str(n.(map[string]any)["uuid"])
		}
		if len(copied) != 2 || copied["Checklist"] == "" || copied["Secret step"] == "" {
			t.Fatalf("the copy's subpages: %v", kids.body)
		}
		// Shared with the copy beside Steps, in the same write: a step bob may edit.
		if lvl := levelOf("zed", copied["Checklist"]); lvl != "edit" {
			t.Errorf("the copy of a step bob may edit is %s to the workspace", lvl)
		}
		if lvl := levelOf("zed", copied["Secret step"]); lvl != "none" {
			t.Errorf("the copy of a step bob may only view is %s to the workspace", lvl)
		}
	})

	t.Run("Restricted through the editor is full access's, and closes the sockets", func(t *testing.T) {
		memo := create("alice", "Memo", "", map[string]any{"doc_private": false})
		share(memo, map[string]any{"adds": []any{
			map[string]any{"kind": "user", "uuid": people["bob"].String(), "level": "full"},
			map[string]any{"kind": "user", "uuid": people["cy"].String(), "level": "edit"},
		}})
		restricted := map[string]any{"doc_uuid": memo, "doc_private": true, "doc_public_comment": false}
		// Refused as the sharing panel refuses it.
		if got := call("cy", "POST", "/doc/updateDoc", docController.UpdateDocBodyLive, restricted, nil); got.code != http.StatusForbidden || str(got.body["code"]) != "forbidden" {
			t.Errorf("an editor makes it Restricted: %d %v", got.code, got.body)
		}
		if levelOf("zed", memo) != "view" {
			t.Fatal("an editor's refused change was made")
		}
		if got := call("bob", "POST", "/doc/updateDoc", docController.UpdateDocBodyLive, restricted, nil); got.code != 200 {
			t.Fatalf("full access makes it Restricted: %d %v", got.code, got.body)
		}
		if levelOf("zed", memo) != "none" || levelOf("cy", memo) != "edit" {
			t.Errorf("after Restricted: zed %s, cy %s", levelOf("zed", memo), levelOf("cy", memo))
		}
		if !sockets.closedFor(memo, people["zed"].String(), 5*time.Second) {
			t.Errorf("open sockets on the memo stay open: %v", sockets.asked[memo])
		}
	})
}
