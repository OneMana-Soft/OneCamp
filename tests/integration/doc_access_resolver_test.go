//go:build integration
// +build integration

package integration_test

// A5's acceptance, through the handlers, against real Postgres 12, Dgraph and
// Redis (search answered by a stub): every source of access and every
// modifier (owner, full access, people, teams, a space, general access, a
// hard and a soft lock, the trash), the batch Filter, a change explained and
// made (until a date, with the expiry sweep; to a page and its subpages in
// one transaction), handing a doc on, an admin's recovery, asking for access,
// removing a comment as a moderator, the live socket's authorisation, A9's
// suggestions on a locked or deleted doc, MQTT, A3's move with "adopt", and
// the access migration down and up again.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDocAccessResolver -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	mqttAccess "github.com/akashc777/OneCamp/business/MqttAccess"
	proposalBusiness "github.com/akashc777/OneCamp/business/Proposal"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// accessMigration finds the access migration's number by its name, so a renumbering
// at merge moves this test with it.
func accessMigration(t *testing.T) (dir string, version uint) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir = filepath.Join(filepath.Dir(file), "..", "..", "migrations")
	matches, err := filepath.Glob(filepath.Join(dir, "*_access_requests_and_expiry.up.sql"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("the access migration: %v %v", matches, err)
	}
	n, err := strconv.Atoi(strings.SplitN(filepath.Base(matches[0]), "_", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	return dir, uint(n)
}

func TestDocAccessResolver(t *testing.T) {
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

	// Olu owns the docs; Ann, Bea, Cy and Di are shared with by name; Eve and
	// Fay are in the Design team (a member and an admin); Olu and Gus are in
	// project P, so Olu may make P's notes (a page goes into a space only by
	// someone in it); Zed has nothing; Ada is a workspace admin.
	names := []string{"olu", "ann", "bea", "cy", "di", "eve", "fay", "gus", "zed", "ada"}
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
	design, project := uuid.NewString(), uuid.NewString()
	nodes = append(nodes,
		map[string]any{"uid": "_:design", "dgraph.type": "Team", "team_uuid": design, "team_name": "Design",
			"team_deleted_at": "0001-01-01T00:00:00Z",
			"team_members":    []map[string]any{{"uid": "_:eve"}}, "team_admins": []map[string]any{{"uid": "_:fay"}}},
		map[string]any{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project, "project_name": "P",
			"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": []map[string]any{{"uid": "_:olu"}, {"uid": "_:gus"}}},
	)
	uids := dg.Mutate(t, nodes)

	as := func(who string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserPostgresInfo.EmailID = people[who].String() + "@example.test"
		u.UserPostgresInfo.IsAdmin = who == "ada"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[who], Uuid: people[who].String(), UserName: who}
		return u
	}
	subject := func(who string) accessBusiness.Subject {
		return accessBusiness.Subject{UserUID: uids[who], UserUUID: people[who].String()}
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
	access := func(who, doc string) answer {
		return call(who, "GET", "/doc/access/"+doc, docController.GetDocAccess, nil, map[string]string{"doc_uuid": doc})
	}
	levelOf := func(who, doc string) string {
		t.Helper()
		a := access(who, doc)
		switch a.code {
		case 200:
			return str(a.data()["level"])
		case 404:
			return "none"
		case 410:
			return "gone"
		}
		t.Fatalf("%s's access to %s: %d %v", who, doc, a.code, a.body)
		return ""
	}
	update := func(who string, body map[string]any) answer {
		return call(who, "POST", "/doc/access/update", docController.UpdateDocAccess, body, nil)
	}
	user := func(n, lvl string) map[string]any {
		return map[string]any{"kind": "user", "uuid": people[n].String(), "level": lvl}
	}
	team := func(lvl string) map[string]any { return map[string]any{"kind": "team", "uuid": design, "level": lvl} }
	expect := func(t *testing.T, doc string, want map[string]string) {
		t.Helper()
		for who, lvl := range want {
			if got := levelOf(who, doc); got != lvl {
				t.Errorf("%s: %s, want %s", who, got, lvl)
			}
		}
	}

	plan := create("olu", "Plan", "", nil)

	t.Run("every source, one by one", func(t *testing.T) {
		got := update("olu", map[string]any{"doc_uuid": plan, "adds": []any{
			user("ann", "full"), user("bea", "edit"), user("cy", "comment"), user("di", "view"), team("comment"),
		}})
		if got.code != 200 {
			t.Fatalf("sharing: %d %v", got.code, got.body)
		}
		expect(t, plan, map[string]string{"olu": "full", "ann": "full", "bea": "edit", "cy": "comment", "di": "view",
			"eve": "comment", "fay": "comment", "gus": "none", "zed": "none", "ada": "none"})
		reasons := func(who string) []any { return list(access(who, plan).data()["reasons"]) }
		if !slices.Contains(reasons("olu"), any("owner")) || !slices.Contains(reasons("eve"), any("team_comment")) || !slices.Contains(reasons("ann"), any("full_access")) {
			t.Errorf("reasons: olu %v eve %v ann %v", reasons("olu"), reasons("eve"), reasons("ann"))
		}
		// Opening the doc: a team's commenter may comment, as the app reads it.
		info := call("eve", "GET", "/doc/getDocInfo/"+plan, docController.GetDocPage, nil, map[string]string{"doc_uuid": plan})
		if info.code != 200 || num(info.data()["doc_comment_access"]) != 1 || num(info.data()["doc_edit_access"]) != 0 {
			t.Errorf("eve opens it: %d %v", info.code, info.data())
		}
		// The sharing panel is for the people who may edit; a commenter sees
		// only their own level.
		if s, ok := access("bea", plan).data()["sharing"].(map[string]any); !ok || len(list(s["people"])) != 4 || len(list(s["teams"])) != 1 {
			t.Errorf("bea's panel: %v", access("bea", plan).data()["sharing"])
		}
		if _, ok := access("cy", plan).data()["sharing"]; ok {
			t.Error("a commenter sees who else has the doc")
		}
		// A doc shared with a team someone is in is shared with them: in
		// their Shared with me list and the tree's Shared section (A3).
		titles := func(a answer, key string) []string {
			var out []string
			for _, n := range list(a.data()[key]) {
				m := n.(map[string]any)
				out = append(out, str(m["doc_title"])+str(m["title"]))
			}
			return out
		}
		for who, want := range map[string]bool{"eve": true, "fay": true, "zed": false} {
			shared := call(who, "GET", "/doc/list?scope=shared", docController.GetDocList, nil, nil)
			tree := call(who, "GET", "/doc/children?space=shared", docController.GetDocChildren, nil, nil)
			if slices.Contains(titles(shared, "docs"), "Plan") != want || slices.Contains(titles(tree, "nodes"), "Plan") != want {
				t.Errorf("%s's shared list %v and tree %v, want Plan there: %v", who, titles(shared, "docs"), titles(tree, "nodes"), want)
			}
		}
	})

	t.Run("general access: the workspace, at view, comment or edit", func(t *testing.T) {
		for lvl, want := range map[string]string{"view": "view", "comment": "comment", "edit": "edit"} {
			if got := update("olu", map[string]any{"doc_uuid": plan, "general": map[string]any{"level": lvl}}); got.code != 200 {
				t.Fatalf("general %s: %d %v", lvl, got.code, got.body)
			}
			expect(t, plan, map[string]string{"zed": want, "gus": want, "di": map[string]string{"view": "view", "comment": "comment", "edit": "edit"}[lvl]})
		}
		priv := true
		if got := update("olu", map[string]any{"doc_uuid": plan, "general": map[string]any{"level": "none", "private": priv}}); got.code != 200 {
			t.Fatalf("private again: %d %v", got.code, got.body)
		}
		expect(t, plan, map[string]string{"zed": "none", "gus": "none", "di": "view"})
	})

	t.Run("a space's members, one hop", func(t *testing.T) {
		spaced := create("olu", "Project notes", "", map[string]any{"space": "project:" + project})
		under := create("olu", "Under it", spaced, nil)
		// A doc in P's space is P's members' to read, and nobody else's.
		expect(t, spaced, map[string]string{"gus": "view", "zed": "none"})
		// A subpage is in its parent's space: its own, so one hop still.
		expect(t, under, map[string]string{"gus": "view", "zed": "none"})
		if got := update("olu", map[string]any{"doc_uuid": spaced, "general": map[string]any{"level": "edit", "private": true}}); got.code != 200 {
			t.Fatalf("space can edit: %d %v", got.code, got.body)
		}
		expect(t, spaced, map[string]string{"gus": "edit", "zed": "none"})
		// A page moved out of the space, and its subpages, leave its members.
		if got := call("olu", "POST", "/doc/move", docController.MoveDoc, map[string]any{"doc_uuid": spaced, "space": "private"}, nil); got.code != 200 {
			t.Fatalf("out of the space: %d %v", got.code, got.body)
		}
		expect(t, spaced, map[string]string{"gus": "none"})
		expect(t, under, map[string]string{"gus": "none"})
	})

	t.Run("filter decides many docs in one read, in their order", func(t *testing.T) {
		var res []accessBusiness.Resource
		var shared []string
		for i := 0; i < 12; i++ {
			d := create("olu", fmt.Sprintf("Batch %d", i), "", nil)
			if i%3 == 0 {
				if got := update("olu", map[string]any{"doc_uuid": d, "adds": []any{user("zed", "view")}}); got.code != 200 {
					t.Fatal(got.body)
				}
				shared = append(shared, d)
			}
			res = append(res, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: d})
		}
		res = append(res, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: uuid.NewString()})
		kept, err := accessBusiness.Rules.Filter(ctx, subject("zed"), res, accessBusiness.LevelView)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range kept {
			got = append(got, r.UUID)
		}
		if !slices.Equal(got, shared) {
			t.Errorf("zed may open %v, want %v", got, shared)
		}
		all, err := accessBusiness.ResolveAll(ctx, subject("olu"), res)
		if err != nil || len(all) != 12 {
			t.Errorf("olu's decisions: %d (%v); a doc that isn't there is left out", len(all), err)
		}
	})

	t.Run("a change explained before it is made", func(t *testing.T) {
		got := call("olu", "POST", "/doc/access/preview", docController.PreviewDocAccess, map[string]any{
			"doc_uuid": plan, "removes": []any{map[string]any{"kind": "team", "uuid": design}},
		}, nil)
		if got.code != 200 || num(got.data()["lose"]) != 2 || num(got.data()["gain"]) != 0 {
			t.Fatalf("taking the team off: %d %v", got.code, got.data())
		}
		got = call("olu", "POST", "/doc/access/preview", docController.PreviewDocAccess, map[string]any{
			"doc_uuid": plan, "general": map[string]any{"level": "view"},
		}, nil)
		if e, _ := got.data()["everyone"].(map[string]any); got.code != 200 || str(e["from"]) != "none" || str(e["to"]) != "view" {
			t.Fatalf("opening it to the workspace: %d %v", got.code, got.data())
		}
		if levelOf("eve", plan) != "comment" || levelOf("zed", plan) != "none" {
			t.Error("a preview changed something")
		}
		// Only the owner and full access may make or preview a change.
		if got := call("bea", "POST", "/doc/access/preview", docController.PreviewDocAccess, map[string]any{"doc_uuid": plan}, nil); got.code != 403 {
			t.Errorf("an editor previews: %d", got.code)
		}
		if got := update("bea", map[string]any{"doc_uuid": plan, "adds": []any{user("zed", "edit")}}); got.code != 403 {
			t.Errorf("an editor shares: %d %v", got.code, got.body)
		}
		if got := update("ann", map[string]any{"doc_uuid": plan, "adds": []any{user("zed", "view")}, "removes": []any{map[string]any{"uuid": people["zed"].String()}}}); got.code != 200 {
			t.Errorf("full access shares: %d %v", got.code, got.body)
		}
	})

	t.Run("access until a date ends when the sweep finds it", func(t *testing.T) {
		until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		got := update("olu", map[string]any{"doc_uuid": plan, "adds": []any{map[string]any{"kind": "user", "uuid": people["zed"].String(), "level": "edit", "until": until}}})
		if got.code != 200 || levelOf("zed", plan) != "edit" {
			t.Fatalf("until an hour from now: %d %v, zed %s", got.code, got.body, levelOf("zed", plan))
		}
		panel := access("olu", plan).data()["sharing"].(map[string]any)
		found := false
		for _, p := range list(panel["people"]) {
			m := p.(map[string]any)
			if str(m["user_uuid"]) == people["zed"].String() && str(m["until"]) != "" {
				found = true
			}
		}
		if !found {
			t.Fatalf("the panel doesn't say when zed's access ends: %v", panel["people"])
		}
		// Not yet: the sweep leaves it.
		if err := accessBusiness.EndDueGrants(ctx); err != nil || levelOf("zed", plan) != "edit" {
			t.Fatalf("an hour early: %v, zed %s", err, levelOf("zed", plan))
		}
		if _, err := env.PG.Exec(`UPDATE access_grant_expiry SET expires_at = NOW() - interval '1 minute' WHERE principal_uuid = $1`, people["zed"]); err != nil {
			t.Fatal(err)
		}
		if err := accessBusiness.EndDueGrants(ctx); err != nil {
			t.Fatal(err)
		}
		if levelOf("zed", plan) != "none" {
			t.Errorf("an ended grant still opens the doc: %s", levelOf("zed", plan))
		}
		var left int
		_ = env.PG.QueryRow(`SELECT count(*) FROM access_grant_expiry`).Scan(&left)
		if left != 0 {
			t.Errorf("%d rows left after the sweep", left)
		}
		// A date that has passed is refused.
		if got := update("olu", map[string]any{"doc_uuid": plan, "adds": []any{map[string]any{"uuid": people["zed"].String(), "level": "view", "until": time.Now().Add(-time.Hour)}}}); got.code != 400 {
			t.Errorf("a date that has passed: %d", got.code)
		}
	})

	t.Run("a hard lock holds editors at comment, on the server and the live socket", func(t *testing.T) {
		// The Doc Session (A2) lets everyone who may read join, read-only
		// unless they may edit; the older authorize route lets in only who
		// may edit, for a collaboration service from before the session.
		session := func(who string) answer {
			return call(who, "POST", "/docColab/session", docController.DocCollabSession, map[string]any{"doc_uuid": plan}, nil)
		}
		authorize := func(who string) answer {
			return call(who, "POST", "/docColab/authorize", docController.AuthorizeDocSocket, map[string]any{"doc_uuid": plan}, nil)
		}
		if a := session("bea"); a.code != 200 || a.body["canEdit"] != true || authorize("bea").code != 200 {
			t.Fatalf("bea before the lock: %d %v", a.code, a.body)
		}
		if got := call("bea", "POST", "/doc/lock", docController.LockDoc, map[string]any{"doc_uuid": plan, "lock": "hard"}, nil); got.code != 403 {
			t.Fatalf("an editor locks hard: %d", got.code)
		}
		if got := call("bea", "POST", "/doc/lock", docController.LockDoc, map[string]any{"doc_uuid": plan, "lock": "soft"}, nil); got.code != 200 || levelOf("bea", plan) != "edit" {
			t.Fatalf("an editor locks soft: %d, bea %s", got.code, levelOf("bea", plan))
		}
		if got := call("ann", "POST", "/doc/lock", docController.LockDoc, map[string]any{"doc_uuid": plan, "lock": "hard"}, nil); got.code != 200 {
			t.Fatalf("full access locks hard: %d %v", got.code, got.body)
		}
		a := access("bea", plan).data()
		if str(a["level"]) != "comment" || a["locked"] != true || str(a["lock"]) != "hard" {
			t.Errorf("bea under a hard lock: %v", a)
		}
		// The web app decides from the doc's counts: Bea's say she may
		// comment, not edit, so the app opens it read-only.
		info := call("bea", "GET", "/doc/getDocInfo/"+plan, docController.GetDocPage, nil, map[string]string{"doc_uuid": plan}).data()
		if num(info["doc_edit_access"]) != 0 || num(info["doc_comment_access"]) != 1 {
			t.Errorf("bea's counts under a hard lock: edit %v comment %v", info["doc_edit_access"], info["doc_comment_access"])
		}
		if a := session("bea"); a.code != 200 || a.body["canEdit"] != false || a.body["locked"] != true || str(a.body["level"]) != "comment" {
			t.Errorf("bea's session under a hard lock: %d %v", a.code, a.body)
		}
		if a := authorize("bea"); a.code != 403 {
			t.Errorf("bea's socket, from a service from before the session: %d", a.code)
		}
		if a := session("ann"); a.code != 200 || a.body["canEdit"] != true {
			t.Errorf("full access under a hard lock: %d %v", a.code, a.body)
		}
		if a := session("di"); a.code != 200 || a.body["canEdit"] != false {
			t.Errorf("a viewer joins read-only: %d %v", a.code, a.body)
		}
		if a, b := session("zed"), authorize("zed"); a.code != 403 || b.code != 403 {
			t.Errorf("someone with nothing joins: %d %d", a.code, b.code)
		}
		// A9: a locked doc takes no suggestions (423 at the handler).
		_, err := proposalBusiness.Proposals.Create(ctx, subject("cy"), proposalBusiness.Proposal{ResourceKind: "doc", ResourceUUID: plan})
		if !errors.Is(err, proposalBusiness.ErrLocked) {
			t.Errorf("a suggestion on a locked doc: %v, want ErrLocked", err)
		}
		if got := call("bea", "POST", "/doc/lock", docController.LockDoc, map[string]any{"doc_uuid": plan, "lock": "none"}, nil); got.code != 403 {
			t.Errorf("an editor lifts a hard lock: %d", got.code)
		}
		if got := call("olu", "POST", "/doc/lock", docController.LockDoc, map[string]any{"doc_uuid": plan, "lock": "none"}, nil); got.code != 200 || levelOf("bea", plan) != "edit" {
			t.Errorf("the owner lifts it: %d, bea %s", got.code, levelOf("bea", plan))
		}
	})

	t.Run("a deleted doc is gone for everyone, and was for the trash", func(t *testing.T) {
		gone := create("olu", "Gone", "", nil)
		if got := update("olu", map[string]any{"doc_uuid": gone, "adds": []any{user("bea", "edit")}}); got.code != 200 {
			t.Fatal(got.body)
		}
		if got := call("olu", "POST", "/doc/delete", docController.DeleteDocTree, map[string]any{"doc_uuid": gone}, nil); got.code != 200 {
			t.Fatalf("delete: %d %v", got.code, got.body)
		}
		expect(t, gone, map[string]string{"olu": "gone", "bea": "gone", "zed": "none"})
		d, err := accessBusiness.Rules.Resolve(ctx, subject("bea"), accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: gone})
		if err != nil || !d.Deleted || d.Level != accessBusiness.LevelNone || d.Was != accessBusiness.LevelEdit {
			t.Errorf("bea's decision on a deleted doc: %+v %v", d, err)
		}
		// Gone is final for the live session, as for the doc page: 410 to
		// whoever it was there for, 404 to anyone else.
		for who, want := range map[string]int{"olu": 410, "bea": 410, "zed": 404} {
			s := call(who, "POST", "/docColab/session", docController.DocCollabSession, map[string]any{"doc_uuid": gone}, nil)
			a := call(who, "POST", "/docColab/authorize", docController.AuthorizeDocSocket, map[string]any{"doc_uuid": gone}, nil)
			info := call(who, "GET", "/doc/getDocInfo/"+gone, docController.GetDocPage, nil, map[string]string{"doc_uuid": gone})
			if s.code != want || a.code != want || info.code != want {
				t.Errorf("%s joining or opening a deleted doc: session %d, authorize %d, page %d, want %d", who, s.code, a.code, info.code, want)
			}
		}
		if got := update("olu", map[string]any{"doc_uuid": gone, "adds": []any{user("zed", "view")}}); got.code != 410 {
			t.Errorf("sharing a deleted doc: %d", got.code)
		}
		if got := call("zed", "POST", "/doc/access/request", docController.RequestDocAccess, map[string]any{"doc_uuid": gone}, nil); got.code != 404 {
			t.Errorf("asking for a deleted doc you never had: %d", got.code)
		}
		// A9: a deleted doc's suggestions read as not found.
		if _, err := proposalBusiness.Proposals.Create(ctx, subject("bea"), proposalBusiness.Proposal{ResourceKind: "doc", ResourceUUID: gone}); !errors.Is(err, proposalBusiness.ErrNotFound) {
			t.Errorf("a suggestion on a deleted doc: %v, want ErrNotFound", err)
		}
	})

	t.Run("a change to a page and its subpages lands in one transaction", func(t *testing.T) {
		child := create("olu", "Child", plan, nil)
		grand := create("bea", "Grandchild", child, nil)
		secret := create("bea", "Secret", grand, nil)
		// The child took the plan's sharing; Bea's pages give the owner of
		// the page above full access.
		expect(t, child, map[string]string{"ann": "full", "bea": "edit", "eve": "comment"})
		expect(t, grand, map[string]string{"olu": "full", "bea": "full"})
		// Bea keeps Secret from Olu.
		if got := update("bea", map[string]any{"doc_uuid": secret, "removes": []any{map[string]any{"uuid": people["olu"].String()}}}); got.code != 200 {
			t.Fatalf("bea keeps secret: %d %v", got.code, got.body)
		}
		got := update("olu", map[string]any{"doc_uuid": child, "adds": []any{user("zed", "edit")}, "apply_to_subpages": true})
		if got.code != 200 || num(got.data()["pages"]) != 2 {
			t.Fatalf("to the child and its subpages: %d %v", got.code, got.data())
		}
		expect(t, child, map[string]string{"zed": "edit"})
		expect(t, grand, map[string]string{"zed": "edit"})
		expect(t, secret, map[string]string{"zed": "none", "olu": "none"})
	})

	t.Run("a doc is handed on, and its owner before keeps full access", func(t *testing.T) {
		handed := create("olu", "Handed", "", nil)
		if got := call("olu", "POST", "/doc/access/transfer", docController.TransferDoc, map[string]any{"doc_uuid": handed, "to_user_uuid": people["bea"].String()}, nil); got.code != 200 {
			t.Fatalf("hand to bea: %d %v", got.code, got.body)
		}
		if a := access("bea", handed).data(); a["owner"] != true || str(a["level"]) != "full" {
			t.Errorf("bea after the hand-over: %v", a)
		}
		if a := access("olu", handed).data(); a["owner"] != false || str(a["level"]) != "full" {
			t.Errorf("olu after the hand-over: %v", a)
		}
		perm := call("olu", "GET", "/doc/getDocPermissions?doc_uuid="+handed, docController.GetDocSharing, nil, nil)
		if owner, _ := perm.data()["doc_created_by"].(map[string]any); perm.code != 200 || str(owner["user_uuid"]) != people["bea"].String() {
			t.Errorf("the share dialog's owner: %d %v", perm.code, perm.data())
		}
		if got := call("olu", "POST", "/doc/access/transfer", docController.TransferDoc, map[string]any{"doc_uuid": handed, "to_user_uuid": people["olu"].String()}, nil); got.code != 403 {
			t.Errorf("the owner before hands it back: %d", got.code)
		}
		if got := call("bea", "POST", "/doc/access/transfer", docController.TransferDoc, map[string]any{"doc_uuid": handed, "to_user_uuid": uuid.NewString()}, nil); got.code != 400 {
			t.Errorf("handing it to someone not in the workspace: %d", got.code)
		}
		// Bea, the owner now, takes Olu off: Olu's legacy reads agree.
		if got := update("bea", map[string]any{"doc_uuid": handed, "removes": []any{map[string]any{"uuid": people["olu"].String()}}}); got.code != 200 || levelOf("olu", handed) != "none" {
			t.Errorf("the maker taken off: %d, olu %s", got.code, levelOf("olu", handed))
		}
		info := call("olu", "GET", "/doc/getDocInfo/"+handed, docController.GetDocPage, nil, map[string]string{"doc_uuid": handed})
		if info.code != 403 && info.code != 404 {
			t.Errorf("its maker opens a doc handed on and taken from them: %d", info.code)
		}
	})

	t.Run("an admin recovers a doc, loudly", func(t *testing.T) {
		left := create("cy", "Left behind", "", nil)
		if got := call("bea", "POST", "/admin/docs/recover", docController.RecoverDoc, map[string]any{"doc_uuid": left, "reason": "Cy left"}, nil); got.code != 403 {
			t.Errorf("a member recovers: %d", got.code)
		}
		if got := call("ada", "POST", "/admin/docs/recover", docController.RecoverDoc, map[string]any{"doc_uuid": left}, nil); got.code != 400 {
			t.Errorf("a recovery without a reason: %d", got.code)
		}
		if got := call("ada", "POST", "/admin/docs/recover", docController.RecoverDoc, map[string]any{"doc_uuid": left, "to_user_uuid": people["di"].String(), "reason": "Cy left"}, nil); got.code != 200 {
			t.Fatalf("recover to di: %d %v", got.code, got.body)
		}
		if a := access("di", left).data(); a["owner"] != true {
			t.Errorf("di after the recovery: %v", a)
		}
		panel, _ := access("cy", left).data()["sharing"].(map[string]any)
		recs := list(panel["recoveries"])
		if len(recs) != 1 || str(recs[0].(map[string]any)["reason"]) != "Cy left" {
			t.Errorf("the owner before doesn't see the recovery: %v", panel)
		}
	})

	t.Run("asking for access, and the answer", func(t *testing.T) {
		asked := create("olu", "Asked", "", nil)
		got := call("zed", "POST", "/doc/access/request", docController.RequestDocAccess, map[string]any{"doc_uuid": asked, "level": "edit", "note": "For the review"}, nil)
		if got.code != 200 {
			t.Fatalf("zed asks: %d %v", got.code, got.body)
		}
		mine := call("zed", "GET", "/doc/access/requests", docController.ListDocAccessRequests, nil, nil)
		out := list(mine.data()["outgoing"])
		if len(out) != 1 || str(out[0].(map[string]any)["doc_title"]) != "" {
			t.Errorf("zed's own requests (never the title of a doc they can't open): %v", out)
		}
		theirs := call("olu", "GET", "/doc/access/requests?doc_uuid="+asked, docController.ListDocAccessRequests, nil, nil)
		in := list(theirs.data()["incoming"])
		if len(in) != 1 || str(in[0].(map[string]any)["doc_title"]) != "Asked" || str(in[0].(map[string]any)["note"]) != "For the review" {
			t.Fatalf("olu's incoming: %v", theirs.data())
		}
		id := str(in[0].(map[string]any)["id"])
		if got := call("bea", "POST", "/doc/access/answer", docController.AnswerDocAccess, map[string]any{"request_id": id, "approve": true}, nil); got.code != 404 {
			t.Errorf("someone who can't open the doc answers: %d", got.code)
		}
		approved := call("olu", "POST", "/doc/access/answer", docController.AnswerDocAccess, map[string]any{"request_id": id, "approve": true, "level": "view"}, nil)
		if approved.code != 200 {
			t.Fatalf("olu approves at view: %d %v", approved.code, approved.body)
		}
		// The answer is the request as the requests list shows it.
		if a := approved.data(); str(a["id"]) != id || str(a["doc_uuid"]) != asked || str(a["status"]) != "approved" ||
			str(a["level_wanted"]) != "edit" || str(a["level_granted"]) != "view" || a["decided_at"] == nil || a["resource_uuid"] != nil {
			t.Errorf("the answer to olu's approval: %v", a)
		}
		if levelOf("zed", asked) != "view" {
			t.Errorf("zed after the answer: %s", levelOf("zed", asked))
		}
		if got := call("olu", "POST", "/doc/access/answer", docController.AnswerDocAccess, map[string]any{"request_id": id, "approve": false}, nil); got.code != 404 {
			t.Errorf("answering twice: %d", got.code)
		}
		if got := call("zed", "POST", "/doc/access/request", docController.RequestDocAccess, map[string]any{"doc_uuid": asked, "level": "view"}, nil); got.code != 409 {
			t.Errorf("asking for what you have: %d", got.code)
		}
	})

	t.Run("a moderator removes anyone's comment", func(t *testing.T) {
		talk := create("olu", "Talk", "", nil)
		if got := update("olu", map[string]any{"doc_uuid": talk, "adds": []any{user("cy", "comment"), user("di", "view"), user("ann", "full")}}); got.code != 200 {
			t.Fatal(got.body)
		}
		c := call("cy", "POST", "/doc/createComment", docController.CreateDocComment, map[string]any{"doc_uuid": talk, "doc_comment_body": "<p>rude</p>"}, nil)
		id := str(c.data()["comment_id"])
		if c.code != 200 || id == "" {
			t.Fatalf("cy comments: %d %v", c.code, c.body)
		}
		remove := func(who string) int {
			return call(who, "POST", "/doc/removeComment", docController.RemoveDocComment, map[string]any{"doc_comment_uuid": id, "doc_uuid": talk}, nil).code
		}
		if code := remove("di"); code != 403 {
			t.Errorf("a viewer removes it: %d", code)
		}
		if code := remove("zed"); code != 404 {
			t.Errorf("someone who can't open the doc removes it: %d", code)
		}
		if code := remove("ann"); code != 200 {
			t.Errorf("full access removes it: %d", code)
		}
	})

	t.Run("a revoked subscription is refused", func(t *testing.T) {
		topic := helpers.GetMqttTopicForDoc(plan)
		ask := func(who string) bool {
			return mqttAccess.Allowed(ctx, mqttAccess.Request{Username: "user_" + people[who].String(), Topic: topic, Action: "subscribe"})
		}
		if !ask("di") || !ask("eve") || ask("zed") {
			t.Fatalf("before: di %v eve %v zed %v", ask("di"), ask("eve"), ask("zed"))
		}
		if got := update("olu", map[string]any{"doc_uuid": plan, "removes": []any{map[string]any{"uuid": people["di"].String()}, map[string]any{"kind": "team", "uuid": design}}}); got.code != 200 {
			t.Fatal(got.body)
		}
		if ask("di") || ask("eve") {
			t.Errorf("after: di %v eve %v", ask("di"), ask("eve"))
		}
	})

	t.Run("a moved page adopts its new parent's sharing: exactly it", func(t *testing.T) {
		parent := create("olu", "Team room", "", nil)
		if got := update("olu", map[string]any{"doc_uuid": parent, "adds": []any{team("edit")}}); got.code != 200 {
			t.Fatal(got.body)
		}
		moved := create("olu", "Moved", "", nil)
		if got := update("olu", map[string]any{"doc_uuid": moved, "adds": []any{user("zed", "view")}}); got.code != 200 {
			t.Fatal(got.body)
		}
		preview := call("olu", "POST", "/doc/move/preview", docController.PreviewDocMove, map[string]any{"doc_uuid": moved, "parent_uuid": parent, "access": "adopt"}, nil)
		if preview.code != 200 || num(preview.data()["lose"]) != 1 || num(preview.data()["gain"]) != 2 || preview.data()["can_adopt"] != true {
			t.Fatalf("preview: %d %v", preview.code, preview.data())
		}
		got := call("olu", "POST", "/doc/move", docController.MoveDoc, map[string]any{"doc_uuid": moved, "parent_uuid": parent, "access": "adopt"}, nil)
		if got.code != 200 || str(got.data()["access"]) != "adopt" {
			t.Fatalf("move: %d %v", got.code, got.body)
		}
		expect(t, moved, map[string]string{"zed": "none", "eve": "edit", "fay": "edit", "olu": "full"})
	})

	t.Run("the access migration goes down and up again", func(t *testing.T) {
		dir, version := accessMigration(t)
		m, err := migrate.New("file://"+dir, env.DSN)
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		for v, _, _ := m.Version(); v >= version; v, _, _ = m.Version() {
			if err := m.Steps(-1); err != nil {
				t.Fatal(err)
			}
		}
		var exists bool
		_ = env.PG.QueryRow(`SELECT to_regclass('access_requests') IS NOT NULL`).Scan(&exists)
		if exists {
			t.Fatal("access_requests is still there after down")
		}
		if err := m.Up(); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"access_requests", "access_grant_expiry", "access_recoveries"} {
			_ = env.PG.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists)
			if !exists {
				t.Errorf("%s didn't come back", table)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d_access_requests_and_expiry.down.sql", version))); err != nil {
			t.Error(err)
		}
	})
}
