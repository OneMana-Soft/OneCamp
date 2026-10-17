//go:build integration
// +build integration

package integration_test

// B6's acceptance (a doc's page chrome), through the handlers, against real
// Postgres 12, Dgraph and Redis (search answered by a stub): a doc opens with
// its look, lock and owner, and 403 for someone it isn't shared with; an
// editor changes the look, a viewer can't, and a hard lock keeps an editor off
// it; an editor invites, up to their own level, adding only (they can't
// escalate, lower, end or remove anyone's access, or touch general access,
// subpages or guest links); "Linked from" names only the docs the person may
// open; a doc in the trash shows its page to whoever could open it, and
// nothing to anyone else; a request for access is approved and the doc
// opens; and the demo's own docs keep their sharing from its visitor.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDocPageChrome -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	demoGuard "github.com/akashc777/OneCamp/business/DemoGuard"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestDocPageChrome(t *testing.T) {
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

	// Olu owns the docs; Ann has full access, Bea edits, Cy comments and Di
	// views; Eve is in the Design team; Fay, Gus and Hal have nothing yet;
	// Vis is the demo's shared visitor.
	names := []string{"olu", "ann", "bea", "cy", "di", "eve", "fay", "gus", "hal", "vis"}
	people := map[string]uuid.UUID{}
	email := func(n string) string { return people[n].String() + "@example.test" }
	var nodes []map[string]any
	for _, n := range names {
		people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			people[n], email(n), n, n); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n})
	}
	design := uuid.NewString()
	nodes = append(nodes, map[string]any{"uid": "_:design", "dgraph.type": "Team", "team_uuid": design, "team_name": "Design",
		"team_deleted_at": "0001-01-01T00:00:00Z", "team_members": []map[string]any{{"uid": "_:eve"}}})
	uids := dg.Mutate(t, nodes)

	as := func(who string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserPostgresInfo.EmailID = email(who)
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
	page := func(who, doc string) answer {
		return call(who, "GET", "/doc/getDocInfo/"+doc, docController.GetDocPage, nil, map[string]string{"doc_uuid": doc})
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
	look := func(who string, body map[string]any) answer {
		return call(who, "POST", "/doc/look", docController.SetDocLook, body, nil)
	}
	invite := func(who string, body map[string]any) answer {
		return call(who, "POST", "/doc/access/invite", docController.InviteToDoc, body, nil)
	}
	user := func(n, lvl string) map[string]any {
		return map[string]any{"kind": "user", "uuid": people[n].String(), "level": lvl}
	}
	lock := func(who, doc, l string) answer {
		return call(who, "POST", "/doc/lock", docController.LockDoc, map[string]any{"doc_uuid": doc, "lock": l}, nil)
	}
	uidOf := func(doc string) string {
		t.Helper()
		var out struct {
			Q []struct{ UID string } `json:"q"`
		}
		if err := json.Unmarshal(dg.Query(t, `{ q(func: eq(doc_uuid, "`+doc+`")) { uid } }`), &out); err != nil || len(out.Q) != 1 {
			t.Fatalf("the uid of %s: %v %s", doc, err, dg.Query(t, `{ q(func: eq(doc_uuid, "`+doc+`")) { uid } }`))
		}
		return out.Q[0].UID
	}

	plan := create("olu", "Plan", "", nil)
	if got := call("olu", "POST", "/doc/access/update", docController.UpdateDocAccess, map[string]any{"doc_uuid": plan, "adds": []any{
		user("ann", "full"), user("bea", "edit"), user("cy", "comment"), user("di", "view"),
	}}, nil); got.code != 200 {
		t.Fatalf("sharing the plan: %d %v", got.code, got.body)
	}

	t.Run("a doc opens with its look, lock and owner, and 403 for someone it isn't shared with", func(t *testing.T) {
		a := page("bea", plan)
		if a.code != 200 || str(a.data()["doc_title"]) != "Plan" || num(a.data()["doc_edit_access"]) != 1 {
			t.Fatalf("bea opens it: %d %v", a.code, a.data())
		}
		owner, _ := a.data()["doc_owner"].(map[string]any)
		if str(owner["user_uuid"]) != people["olu"].String() || str(owner["user_name"]) != "olu" {
			t.Errorf("the owner, named: %v", owner)
		}
		if _, ok := a.data()["doc_icon"]; ok {
			t.Errorf("a new doc has an icon: %v", a.data()["doc_icon"])
		}
		no := page("hal", plan)
		if no.code != 403 || str(no.body["code"]) != "no_access" {
			t.Errorf("hal, who it isn't shared with: %d %v", no.code, no.body)
		}
		if got := page("hal", uuid.NewString()); got.code != 404 {
			t.Errorf("no such doc: %d", got.code)
		}
	})

	t.Run("an editor changes the look; a viewer, a commenter and a stranger can't", func(t *testing.T) {
		got := look("bea", map[string]any{"doc_uuid": plan, "icon": "📘", "cover": "gallery:dusk", "cover_offset": 30, "font": "serif", "small_text": true, "full_width": true})
		if got.code != 200 {
			t.Fatalf("bea restyles it: %d %v", got.code, got.body)
		}
		d := page("di", plan).data()
		if str(d["doc_icon"]) != "📘" || str(d["doc_cover_key"]) != "gallery:dusk" || num(d["doc_cover_offset"]) != 30 ||
			str(d["doc_font"]) != "serif" || d["doc_small_text"] != true || d["doc_full_width"] != true {
			t.Errorf("di sees: %v", d)
		}
		// The icon is the page's everywhere it is named: the lists, the tree.
		mine := call("olu", "GET", "/doc/list?scope=mine", docController.GetDocList, nil, nil)
		found := false
		for _, row := range list(mine.data()["docs"]) {
			if m := row.(map[string]any); str(m["doc_uuid"]) == plan {
				found = str(m["doc_icon"]) == "📘"
			}
		}
		if !found {
			t.Errorf("olu's list doesn't show the icon: %v", mine.data())
		}
		if got := look("bea", map[string]any{"doc_uuid": plan, "icon": "glyph:rocket:sky"}); got.code != 200 || str(page("di", plan).data()["doc_icon"]) != "glyph:rocket:sky" {
			t.Errorf("a glyph: %d %v", got.code, got.body)
		}
		// Taking the cover away takes its position with it.
		if got := look("bea", map[string]any{"doc_uuid": plan, "icon": "", "cover": "", "font": ""}); got.code != 200 {
			t.Fatalf("plain again: %d %v", got.code, got.body)
		}
		d = page("di", plan).data()
		for _, f := range []string{"doc_icon", "doc_cover_key", "doc_cover_offset", "doc_font"} {
			if _, ok := d[f]; ok {
				t.Errorf("%s is still there: %v", f, d[f])
			}
		}
		for who, want := range map[string]int{"di": 403, "cy": 403, "hal": 404} {
			if got := look(who, map[string]any{"doc_uuid": plan, "icon": "📕"}); got.code != want {
				t.Errorf("%s restyles it: %d, want %d", who, got.code, want)
			}
		}
		for _, bad := range []map[string]any{
			{"icon": "hello"}, {"icon": "<img src=x>"}, {"cover": "https://example.com/a.png"}, {"cover_offset": 120}, {"font": "comic"}, {},
			{"cover": uuid.NewString() + "/" + uuid.NewString()}, // another doc's
			{"cover": plan + "/" + uuid.NewString()},             // not one of its files
		} {
			bad["doc_uuid"] = plan
			if got := look("bea", bad); got.code != 400 || str(got.body["msg"]) == "" {
				t.Errorf("%v: %d %v", bad, got.code, got.body)
			}
		}
		// An image uploaded to the doc is a cover.
		file := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO attachments (id, obj_key, src_value, src_key, created_by) VALUES ($1, 'covers/x.png', $2, 'doc', $3)`, file, plan, people["bea"]); err != nil {
			t.Fatal(err)
		}
		if got := look("bea", map[string]any{"doc_uuid": plan, "cover": plan + "/" + file.String(), "cover_offset": 50}); got.code != 200 {
			t.Errorf("an uploaded cover: %d %v", got.code, got.body)
		}
		if str(page("di", plan).data()["doc_cover_key"]) != plan+"/"+file.String() {
			t.Errorf("the uploaded cover: %v", page("di", plan).data()["doc_cover_key"])
		}
	})

	t.Run("an editor invites, up to their own level, and only adds", func(t *testing.T) {
		if got := invite("bea", map[string]any{"doc_uuid": plan, "adds": []any{user("fay", "edit")}}); got.code != 200 || num(got.data()["gain"]) != 1 {
			t.Fatalf("bea invites fay to edit: %d %v", got.code, got.body)
		}
		if levelOf("fay", plan) != "edit" {
			t.Errorf("fay: %s", levelOf("fay", plan))
		}
		cases := []struct {
			name string
			who  string
			body map[string]any
			code int
			key  string
		}{
			{"an editor can't give full access", "bea", map[string]any{"adds": []any{user("gus", "full")}}, 403, "above_your_level"},
			{"nor change someone who has access (lower di's)", "bea", map[string]any{"adds": []any{user("di", "edit")}}, 409, "already_shared"},
			{"nor take full access down", "bea", map[string]any{"adds": []any{user("ann", "view")}}, 409, "already_shared"},
			{"nor name the owner", "bea", map[string]any{"adds": []any{user("olu", "view")}}, 409, "already_shared"},
			{"nor themselves", "bea", map[string]any{"adds": []any{user("bea", "edit")}}, 409, "already_shared"},
			{"nor take anyone away", "bea", map[string]any{"adds": []any{user("gus", "view")}, "removes": []any{map[string]any{"kind": "user", "uuid": people["di"].String()}}}, 400, ""},
			{"nor open it to everyone", "bea", map[string]any{"adds": []any{user("gus", "view")}, "general": map[string]any{"level": "edit"}}, 400, ""},
			{"nor reach the pages under it", "bea", map[string]any{"adds": []any{user("gus", "view")}, "apply_to_subpages": true}, 400, ""},
			{"nor its guest links", "bea", map[string]any{"adds": []any{user("gus", "view")}, "guest_links": "revoke"}, 400, ""},
			{"a team is at most an editor", "bea", map[string]any{"adds": []any{map[string]any{"kind": "team", "uuid": design, "level": "full"}}}, 400, ""},
			{"a commenter can't invite", "cy", map[string]any{"adds": []any{user("gus", "comment")}}, 403, "forbidden"},
			{"a viewer can't invite", "di", map[string]any{"adds": []any{user("gus", "view")}}, 403, "forbidden"},
			{"someone without access is told nothing", "hal", map[string]any{"adds": []any{user("gus", "view")}}, 404, ""},
		}
		for _, tc := range cases {
			tc.body["doc_uuid"] = plan
			got := invite(tc.who, tc.body)
			if got.code != tc.code || (tc.key != "" && str(got.body["code"]) != tc.key) {
				t.Errorf("%s: %d %v, want %d %s", tc.name, got.code, got.body, tc.code, tc.key)
			}
		}
		for who, want := range map[string]string{"gus": "none", "di": "view", "ann": "full", "bea": "edit"} {
			if got := levelOf(who, plan); got != want {
				t.Errorf("after the refusals %s is %s, want %s", who, got, want)
			}
		}
		// A team, until tomorrow: its members can comment, and the panel says until when.
		until := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
		if got := invite("fay", map[string]any{"doc_uuid": plan, "adds": []any{map[string]any{"kind": "team", "uuid": design, "level": "comment", "until": until}}}); got.code != 200 {
			t.Fatalf("fay invites Design until tomorrow: %d %v", got.code, got.body)
		}
		if levelOf("eve", plan) != "comment" {
			t.Errorf("eve, in Design: %s", levelOf("eve", plan))
		}
		panel := call("olu", "GET", "/doc/access/"+plan, docController.GetDocAccess, nil, map[string]string{"doc_uuid": plan}).data()["sharing"].(map[string]any)
		teams := list(panel["teams"])
		if len(teams) != 1 || str(teams[0].(map[string]any)["until"]) == "" {
			t.Errorf("the team's grant ends: %v", teams)
		}
		// Full access invites at full.
		if got := invite("ann", map[string]any{"doc_uuid": plan, "adds": []any{user("gus", "full")}}); got.code != 200 || levelOf("gus", plan) != "full" {
			t.Errorf("ann gives gus full access: %d %v", got.code, got.body)
		}
	})

	t.Run("a hard lock keeps an editor off the look and off invites", func(t *testing.T) {
		if got := lock("olu", plan, "hard"); got.code != 200 {
			t.Fatalf("olu locks it: %d %v", got.code, got.body)
		}
		got := look("bea", map[string]any{"doc_uuid": plan, "icon": "📕"})
		if got.code != 403 || str(got.body["msg"]) != "This doc is locked. Someone with full access can unlock it." {
			t.Errorf("bea restyles a locked doc: %d %v", got.code, got.body)
		}
		if got := invite("bea", map[string]any{"doc_uuid": plan, "adds": []any{user("hal", "view")}}); got.code != 403 {
			t.Errorf("bea invites to a locked doc: %d %v", got.code, got.body)
		}
		if d := page("bea", plan).data(); str(d["doc_lock_level"]) != "hard" || num(d["doc_edit_access"]) != 0 {
			t.Errorf("bea opens it locked: %v", d)
		}
		if got := look("ann", map[string]any{"doc_uuid": plan, "icon": "📕"}); got.code != 200 {
			t.Errorf("ann, full access, restyles it: %d %v", got.code, got.body)
		}
		if got := lock("olu", plan, "none"); got.code != 200 {
			t.Fatalf("olu unlocks it: %d %v", got.code, got.body)
		}
	})

	t.Run("linked from: the docs that link here, that you may open", func(t *testing.T) {
		open := create("bea", "Open notes", "", map[string]any{"doc_private": false})
		secret := create("olu", "Secret", "", nil)
		gone := create("olu", "Gone", "", nil)
		for _, from := range []string{open, secret, gone} {
			dg.Mutate(t, map[string]any{"uid": uidOf(from), "doc_mentions": []map[string]any{{"uid": uidOf(plan)}}})
		}
		// A doc linking to itself isn't its own backlink.
		dg.Mutate(t, map[string]any{"uid": uidOf(plan), "doc_mentions": []map[string]any{{"uid": uidOf(plan)}}})
		if got := call("olu", "POST", "/doc/delete", docController.DeleteDocTree, map[string]any{"doc_uuid": gone}, nil); got.code != 200 {
			t.Fatalf("deleting Gone: %d %v", got.code, got.body)
		}
		titles := func(who string) []string {
			a := call(who, "GET", "/doc/backlinks/"+plan, docController.GetDocBacklinks, nil, map[string]string{"doc_uuid": plan})
			if a.code != 200 {
				t.Fatalf("%s's backlinks: %d %v", who, a.code, a.body)
			}
			var out []string
			for _, d := range list(a.data()["docs"]) {
				out = append(out, str(d.(map[string]any)["doc_title"]))
			}
			slices.Sort(out)
			if num(a.data()["count"]) != len(out)+len(list(a.data()["tasks"]))+len(list(a.data()["projects"])) {
				t.Errorf("%s's count: %v", who, a.data())
			}
			return out
		}
		if got := titles("olu"); !slices.Equal(got, []string{"Open notes", "Secret"}) {
			t.Errorf("olu: %v", got)
		}
		if got := titles("di"); !slices.Equal(got, []string{"Open notes"}) {
			t.Errorf("di, who can't open Secret: %v", got)
		}
		// The page keeps the row from its first paint: the live docs that link
		// here, itself left out (Gone is in the trash).
		if got := page("olu", plan); num(got.data()["doc_backlink_count"]) != 2 {
			t.Errorf("the page's backlink count: %v", got.data()["doc_backlink_count"])
		}
		if got := call("hal", "GET", "/doc/backlinks/"+plan, docController.GetDocBacklinks, nil, map[string]string{"doc_uuid": plan}); got.code != 404 {
			t.Errorf("hal, without access: %d %v", got.code, got.body)
		}
	})

	t.Run("a doc in the trash shows its page to whoever could open it", func(t *testing.T) {
		old := create("olu", "Old plan", "", nil)
		create("olu", "Under it", old, nil)
		if got := call("olu", "POST", "/doc/access/update", docController.UpdateDocAccess, map[string]any{"doc_uuid": old, "adds": []any{user("di", "view")}}, nil); got.code != 200 {
			t.Fatalf("sharing it: %d %v", got.code, got.body)
		}
		if got := call("olu", "POST", "/doc/delete", docController.DeleteDocTree, map[string]any{"doc_uuid": old}, nil); got.code != 200 {
			t.Fatalf("deleting it: %d %v", got.code, got.body)
		}
		trashed := func(who, doc string) answer {
			return call(who, "GET", "/doc/trashed/"+doc, docController.GetTrashedDoc, nil, map[string]string{"doc_uuid": doc})
		}
		a := trashed("olu", old)
		if a.code != 200 || str(a.data()["doc_title"]) != "Old plan" || num(a.data()["subpages"]) != 1 ||
			a.data()["can_restore"] != true || a.data()["can_purge"] != true || str(a.data()["purge_at"]) == "" {
			t.Errorf("olu: %d %v", a.code, a.data())
		}
		if by, _ := a.data()["deleted_by"].(map[string]any); str(by["user_uuid"]) != people["olu"].String() {
			t.Errorf("who deleted it: %v", a.data()["deleted_by"])
		}
		if a := trashed("di", old); a.code != 200 || a.data()["can_restore"] != false || a.data()["can_purge"] != false {
			t.Errorf("di, who could view it: %d %v", a.code, a.data())
		}
		if a := trashed("hal", old); a.code != 404 {
			t.Errorf("hal, who never could: %d %v", a.code, a.body)
		}
		if a := trashed("olu", plan); a.code != 409 {
			t.Errorf("a live doc: %d %v", a.code, a.body)
		}
		if a := page("olu", old); a.code != 410 || a.body["can_restore"] != true {
			t.Errorf("opening it: %d %v", a.code, a.body)
		}
	})

	t.Run("a request for access is approved, and the doc opens", func(t *testing.T) {
		if got := page("hal", plan); got.code != 403 {
			t.Fatalf("hal opens it first: %d", got.code)
		}
		if got := call("hal", "POST", "/doc/access/request", docController.RequestDocAccess, map[string]any{"doc_uuid": plan, "level": "view", "note": "For the launch"}, nil); got.code != 200 {
			t.Fatalf("hal asks: %d %v", got.code, got.body)
		}
		in := call("olu", "GET", "/doc/access/requests?doc_uuid="+plan, docController.ListDocAccessRequests, nil, nil)
		var id string
		for _, r := range list(in.data()["incoming"]) {
			if m := r.(map[string]any); str(m["note"]) == "For the launch" {
				id = str(m["id"])
			}
		}
		if id == "" {
			t.Fatalf("olu's inbox: %v", in.data())
		}
		if got := call("olu", "POST", "/doc/access/answer", docController.AnswerDocAccess, map[string]any{"request_id": id, "approve": true}, nil); got.code != 200 {
			t.Fatalf("olu approves: %d %v", got.code, got.body)
		}
		if got := page("hal", plan); got.code != 200 {
			t.Errorf("hal opens it after: %d %v", got.code, got.body)
		}
	})

	t.Run("the demo's own docs keep their sharing from its visitor", func(t *testing.T) {
		t.Setenv("DEMO_MODE", "true")
		t.Setenv("DEMO_USER_EMAIL", email("vis"))
		seededAt := time.Now().Add(-time.Hour).UTC()
		if err := demoGuard.MarkSeeded(seededAt); err != nil {
			t.Fatal(err)
		}
		seeded := uuid.NewString()
		dg.Mutate(t, map[string]any{"uid": "_:seeded", "dgraph.type": "Doc", "doc_uuid": seeded, "doc_title": "Q4 launch plan",
			"doc_private": false, "doc_deleted_at": "0001-01-01T00:00:00Z", "doc_created_at": seededAt.Add(-10 * time.Minute).Format(time.RFC3339),
			"doc_created_by": map[string]any{"uid": uids["vis"]}})
		if got := invite("vis", map[string]any{"doc_uuid": seeded, "adds": []any{user("hal", "view")}}); got.code != 403 || str(got.body["code"]) != "demo" {
			t.Errorf("the visitor invites to the demo's doc: %d %v", got.code, got.body)
		}
		mine := create("vis", "My notes", "", nil)
		if got := invite("vis", map[string]any{"doc_uuid": mine, "adds": []any{user("hal", "view")}}); got.code != 200 {
			t.Errorf("the visitor invites to their own doc: %d %v", got.code, got.body)
		}
	})
}
