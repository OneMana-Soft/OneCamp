//go:build integration
// +build integration

package integration_test

// Docs incident, 14 Oct 2026: what a member may do with a doc is the same on
// every surface, whatever their right comes from. Every source of rights
// (authorship, a direct share, general access, a team, a space, full access,
// a hand-over) at every level it gives, against real Postgres 12, Dgraph and
// Redis:
//   - GET /doc/getDocInfo and GET /doc/getCommentList answer the resolved
//     level in the counts the web reads (doc_read/comment/edit/full_access).
//     The comment list answered the doc's direct shares only, so a member who
//     could comment through general access, a team, a space, full access or a
//     hand-over got no comment box;
//   - commenting is open from comment up;
//   - Ask AI's reading actions are open from view, its changes from edit;
//   - GET /doc/list says the level of each doc it lists.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDocAbilitiesMatrix -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestDocAbilitiesMatrix(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"hits":{"total":{"value":0},"hits":[]}}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	names := []string{"olu", "author", "sview", "scomment", "sedit", "sfull", "member", "teammate", "spacer", "maker", "taker"}
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, n := range names {
		people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			people[n], n+"@example.test", n, n); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n})
	}
	team, project := uuid.NewString(), uuid.NewString()
	nodes = append(nodes,
		map[string]any{"uid": "_:team", "dgraph.type": "Team", "team_uuid": team, "team_name": "Design",
			"team_deleted_at": "0001-01-01T00:00:00Z", "team_members": []map[string]any{{"uid": "_:teammate"}}},
		map[string]any{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project, "project_name": "P",
			"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": []map[string]any{{"uid": "_:spacer"}}},
	)
	docs := map[string]string{}
	doc := func(key, by string, private bool, extra map[string]any) {
		docs[key] = uuid.NewString()
		m := map[string]any{"uid": "_:" + key, "dgraph.type": "Doc", "doc_uuid": docs[key], "doc_title": key,
			"doc_private": private, "doc_public_comment": false, "doc_public_edit": false,
			"doc_deleted_at": "0001-01-01T00:00:00Z", "doc_created_at": "2026-10-14T05:00:00Z", "doc_updated_at": "2026-10-14T05:00:00Z",
			"doc_body": "<p>We launch on Tuesday.</p>", "doc_created_by": map[string]any{"uid": "_:" + by}}
		for k, v := range extra {
			m[k] = v
		}
		nodes = append(nodes, m)
	}
	ref := func(n string) []map[string]any { return []map[string]any{{"uid": "_:" + n}} }
	doc("authored", "author", true, nil)
	doc("shared", "olu", true, map[string]any{
		"doc_reading_users": ref("sview"), "doc_commenting_users": ref("scomment"),
		"doc_editing_users": ref("sedit"), "doc_full_access_users": ref("sfull")})
	doc("generalView", "olu", false, nil)
	doc("generalComment", "olu", false, map[string]any{"doc_public_comment": true})
	doc("generalEdit", "olu", false, map[string]any{"doc_public_edit": true})
	doc("teamView", "olu", true, map[string]any{"doc_team_viewers": []map[string]any{{"uid": "_:team"}}})
	doc("teamComment", "olu", true, map[string]any{"doc_team_commenters": []map[string]any{{"uid": "_:team"}}})
	doc("teamEdit", "olu", true, map[string]any{"doc_team_editors": []map[string]any{{"uid": "_:team"}}})
	space := map[string]any{"doc_space_kind": "project", "doc_space_uuid": project}
	doc("spaceView", "olu", true, space)
	doc("spaceComment", "olu", true, map[string]any{"doc_space_kind": "project", "doc_space_uuid": project, "doc_public_comment": true})
	doc("spaceEdit", "olu", true, map[string]any{"doc_space_kind": "project", "doc_space_uuid": project, "doc_public_edit": true})
	// Handed from its maker to taker; the maker keeps full access.
	doc("handed", "maker", true, map[string]any{"doc_owner": map[string]any{"uid": "_:taker"}, "doc_full_access_users": ref("maker")})
	uids := dg.Mutate(t, nodes)

	as := func(who string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserPostgresInfo.EmailID = who + "@example.test"
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
	rank := map[string]int{"none": 0, "view": 1, "comment": 2, "edit": 3, "full": 4}
	countsSay := func(d map[string]any) string {
		n := func(k string) bool { v, _ := d[k].(float64); return v > 0 }
		switch {
		case n("doc_full_access"):
			return "full"
		case n("doc_edit_access"):
			return "edit"
		case n("doc_comment_access"):
			return "comment"
		case n("doc_read_access"):
			return "view"
		}
		return "none"
	}

	cases := []struct{ source, doc, who, want string }{
		{"author", "authored", "author", "full"},
		{"direct share", "shared", "sview", "view"},
		{"direct share", "shared", "scomment", "comment"},
		{"direct share", "shared", "sedit", "edit"},
		{"direct share", "shared", "sfull", "full"},
		{"general access", "generalView", "member", "view"},
		{"general access", "generalComment", "member", "comment"},
		{"general access", "generalEdit", "member", "edit"},
		{"team share", "teamView", "teammate", "view"},
		{"team share", "teamComment", "teammate", "comment"},
		{"team share", "teamEdit", "teammate", "edit"},
		{"space member", "spaceView", "spacer", "view"},
		{"space member", "spaceComment", "spacer", "comment"},
		{"space member", "spaceEdit", "spacer", "edit"},
		{"ownership transfer", "handed", "taker", "full"},
		{"full access (after a hand-over)", "handed", "maker", "full"},
	}
	for _, c := range cases {
		name := fmt.Sprintf("%s %s on %s", c.source, c.who, c.doc)
		id := docs[c.doc]
		info := call(c.who, "GET", "/doc/getDocInfo/"+id, docController.GetDocPage, nil, map[string]string{"doc_uuid": id})
		if info.code != 200 || countsSay(info.data()) != c.want {
			t.Errorf("%s: getDocInfo %d says %s, want %s", name, info.code, countsSay(info.data()), c.want)
		}
		list := call(c.who, "GET", "/doc/getCommentList/"+id, docController.GetAllCommentList, nil, map[string]string{"doc_uuid": id})
		if list.code != 200 || countsSay(list.data()) != c.want {
			t.Errorf("%s: getCommentList %d says %s, want %s (the comment box is offered by it)", name, list.code, countsSay(list.data()), c.want)
		}
		basic, err := docBusiness.GetBasicDgraphDocByUUID(ctx, id, uids[c.who])
		if err != nil {
			t.Fatal(err)
		}
		if got, want := docBusiness.CanComment(basic, people[c.who].String()), rank[c.want] >= rank["comment"]; got != want {
			t.Errorf("%s: may comment %v, want %v", name, got, want)
		}
		// What each Doc AI action needs is what the doc is asked for (the bar's
		// handler refuses by the same check, 403 for a change below edit).
		u := as(c.who)
		for action, need := range map[string]string{"summarize": "view", "explain": "view", "improve": "edit", "continue": "edit"} {
			_, err := aiBusiness.CheckDocForAI(ctx, &u, id, aiBusiness.DocActionNeeds(action))
			allowed := rank[c.want] >= rank[need]
			if allowed && err != nil {
				t.Errorf("%s: Ask AI %s refused: %v", name, action, err)
			}
			if !allowed && !errors.Is(err, aiBusiness.ErrDocAICantEdit) {
				t.Errorf("%s: Ask AI %s answered %v, want refused", name, action, err)
			}
		}
	}

	// Someone with no right at all: no doc, and no Ask AI on it.
	if a := call("member", "GET", "/doc/getCommentList/"+docs["shared"], docController.GetAllCommentList, nil, map[string]string{"doc_uuid": docs["shared"]}); a.code != http.StatusForbidden {
		t.Errorf("a stranger's comment list answered %d, want 403", a.code)
	}
	stranger := as("member")
	if _, err := aiBusiness.CheckDocForAI(ctx, &stranger, docs["shared"], aiBusiness.DocActionNeeds("summarize")); !errors.Is(err, aiBusiness.ErrDocAINotFound) {
		t.Errorf("a stranger's summary answered %v, want not found", err)
	}

	// The workspace list says what the member may do with each doc.
	l := call("member", "GET", "/doc/list?scope=workspace&limit=50", docController.GetDocList, nil, nil)
	if l.code != 200 {
		t.Fatalf("list: %d %v", l.code, l.body)
	}
	levels := map[string]string{}
	rows, _ := l.data()["docs"].([]any)
	for _, r := range rows {
		m, _ := r.(map[string]any)
		levels[fmt.Sprint(m["doc_uuid"])] = fmt.Sprint(m["level"])
	}
	for key, want := range map[string]string{"generalView": "view", "generalComment": "comment", "generalEdit": "edit"} {
		if levels[docs[key]] != want {
			t.Errorf("the list says %s for %s, want %s (all: %v)", levels[docs[key]], key, want, levels)
		}
	}
}

// D3: Request access goes only to someone who can answer. A doc whose
// managers are the demo's own people (its cast, its shared visitor) offers no
// request and refuses one; a doc a real person owns offers it; the demo's
// shared visitor never asks.
func TestDocRequestOnlyToSomeoneWhoAnswers(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")

	emails := map[string]string{
		"maya":    "maya" + helpers.DemoCastEmailDomain,
		"visitor": "visitor@demo.example",
		"real":    "real@company.example",
		"member":  "member@company.example",
	}
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for n, e := range emails {
		people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			people[n], e, n, n); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n})
	}
	docs := map[string]string{}
	for key, owner := range map[string]string{"cast": "maya", "visitors": "visitor", "reals": "real"} {
		docs[key] = uuid.NewString()
		nodes = append(nodes, map[string]any{"uid": "_:" + key, "dgraph.type": "Doc", "doc_uuid": docs[key], "doc_title": key,
			"doc_private": false, "doc_public_comment": false, "doc_deleted_at": "0001-01-01T00:00:00Z",
			"doc_created_by": map[string]any{"uid": "_:" + owner}})
	}
	docs["private"] = uuid.NewString()
	nodes = append(nodes, map[string]any{"uid": "_:private", "dgraph.type": "Doc", "doc_uuid": docs["private"], "doc_title": "private",
		"doc_private": true, "doc_deleted_at": "0001-01-01T00:00:00Z", "doc_created_by": map[string]any{"uid": "_:real"}})
	uids := dg.Mutate(t, nodes)

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
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserPostgresInfo.EmailID = emails[who]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[who], Uuid: people[who].String(), UserName: who}
		c := context.WithValue(ctx, helpers.UserInfoContextKey, u)
		req = req.WithContext(context.WithValue(c, chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		handler(rec, req)
		out := answer{code: rec.Code}
		_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
		return out
	}
	canRequest := func(who, key string) bool {
		t.Helper()
		a := call(who, "GET", "/doc/access/"+docs[key], docController.GetDocAccess, nil, map[string]string{"doc_uuid": docs[key]})
		if a.code != 200 {
			t.Fatalf("%s's access to %s: %d %v", who, key, a.code, a.body)
		}
		v, _ := a.data()["can_request"].(bool)
		return v
	}
	if canRequest("member", "cast") || canRequest("member", "visitors") {
		t.Error("a doc only the demo's own people manage offers Request access")
	}
	if !canRequest("member", "reals") {
		t.Error("a doc a real person owns doesn't offer Request access to a viewer")
	}
	if canRequest("real", "reals") {
		t.Error("its owner is offered Request access")
	}
	ask := func(who, key string) answer {
		return call(who, "POST", "/doc/access/request", docController.RequestDocAccess, map[string]any{"doc_uuid": docs[key], "level": "edit"}, nil)
	}
	if a := ask("member", "cast"); a.code != http.StatusConflict {
		t.Errorf("a request nobody can answer answered %d %v, want 409", a.code, a.body)
	}
	if a := ask("member", "reals"); a.code != 200 {
		t.Errorf("a request its owner can answer answered %d %v", a.code, a.body)
	}
	if a := ask("visitor", "private"); a.code != http.StatusForbidden || a.body["code"] != "demo" {
		t.Errorf("the demo's shared visitor asking answered %d %v, want 403 demo", a.code, a.body)
	}
	// The no-access page says whether a request would reach anyone.
	a := call("member", "GET", "/doc/getDocInfo/"+docs["private"], docController.GetDocPage, nil, map[string]string{"doc_uuid": docs["private"]})
	if a.code != http.StatusForbidden || a.body["can_request"] != true {
		t.Errorf("a private doc a real person owns answered %d %v, want 403 with can_request", a.code, a.body)
	}
	a = call("visitor", "GET", "/doc/getDocInfo/"+docs["private"], docController.GetDocPage, nil, map[string]string{"doc_uuid": docs["private"]})
	if a.code != http.StatusForbidden || a.body["can_request"] != false {
		t.Errorf("the demo visitor's no-access answer %d %v, want can_request false", a.code, a.body)
	}
}
