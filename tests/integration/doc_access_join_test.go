//go:build integration
// +build integration

package integration_test

// The A5×A6 join against real Postgres 12, Dgraph and Redis (search a stub,
// MQTT nowhere, pushes watched where every path sends them):
//   - who may read a doc is the Access Resolver's answer from the doc's side
//     (business/Doc Readers), held equal to its answer person by person (GET
//     /doc/access), in both directions;
//   - a mention reaches someone the doc reaches only through a team, and not
//     someone whose grant has ended (before the sweep, and after it) or who
//     was taken out of the doc's space; whoever mentioned them is asked to
//     share instead;
//   - doc.shared says the level each person was given;
//   - a request for access reaches the people who can answer it, its answer
//     reaches the asker, and a declined asker's notice says nothing of the
//     doc: not in the inbox, the push or the row;
//   - the old POST /doc/updateDocPermissions handler goes by A5's rules.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDocAccessJoin -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	demoGuard "github.com/akashc777/OneCamp/business/DemoGuard"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestDocAccessJoin(t *testing.T) {
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

	// Olu owns the docs. Ann gets full access, Bea edit, Cy view until a
	// moment soon, Di view until much later. Eve and Fay are in the Design
	// team; Olu, Gus and Hal are in project P, so Olu may make P's notes (a
	// page goes into a space only by someone in it); Ivy, then Zed, have
	// nothing.
	names := []string{"olu", "ann", "bea", "cy", "di", "eve", "fay", "gus", "hal", "ivy", "zed"}
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, n := range names {
		people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			people[n], people[n].String()+"@example.test", n, n); err != nil {
			t.Fatal(err)
		}
		if _, err := env.PG.Exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', $2)`, people[n], "device-of-"+n); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n})
	}
	design, project := uuid.NewString(), uuid.NewString()
	nodes = append(nodes,
		map[string]any{"uid": "_:design", "dgraph.type": "Team", "team_uuid": design, "team_name": "Design",
			"team_deleted_at": "0001-01-01T00:00:00Z",
			"team_members":    []map[string]any{{"uid": "_:eve"}}, "team_admins": []map[string]any{{"uid": "_:fay"}}},
		map[string]any{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project, "project_name": "P",
			"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": []map[string]any{{"uid": "_:olu"}, {"uid": "_:gus"}, {"uid": "_:hal"}}},
	)
	uids := dg.Mutate(t, nodes)

	// Pushes about our docs: who, and what they said.
	var mu sync.Mutex
	type push struct{ title, body, to string }
	var pushes []push
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		mu.Lock()
		defer mu.Unlock()
		for _, tok := range tokens {
			pushes = append(pushes, push{data[firebaseInit.FIREBASE_PUSH_DATA_TITLE], data[firebaseInit.FIREBASE_PUSH_DATA_BODY], strings.TrimPrefix(tok, "device-of-")})
		}
	})()
	pushedTo := func(who string) []push {
		mu.Lock()
		defer mu.Unlock()
		var out []push
		for _, p := range pushes {
			if p.to == who {
				out = append(out, p)
			}
		}
		return out
	}

	as := func(who string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserPostgresInfo.EmailID = people[who].String() + "@example.test"
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
	create := func(who, title string, body map[string]any) string {
		t.Helper()
		in := map[string]any{"doc_title": title, "parent_uuid": "", "doc_private": true}
		for k, v := range body {
			in[k] = v
		}
		a := call(who, "POST", "/doc/createDoc", docController.CreateDocInTree, in, nil)
		if a.code != 200 {
			t.Fatalf("%s making %q: %d %v", who, title, a.code, a.body)
		}
		return str(a.data()["doc_uuid"])
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
	user := func(n, lvl string) map[string]any {
		return map[string]any{"kind": "user", "uuid": people[n].String(), "level": lvl}
	}
	update := func(who string, body map[string]any) {
		t.Helper()
		if a := call(who, "POST", "/doc/access/update", docController.UpdateDocAccess, body, nil); a.code != 200 {
			t.Fatalf("%s changing access: %d %v", who, a.code, a.body)
		}
	}
	notices := func(who, kind, doc string) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM resource_notices WHERE user_uuid = $1 AND kind = $2 AND resource_uuid = $3`,
			people[who], kind, doc).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	audited := func(action, doc string) []string {
		t.Helper()
		rows, err := env.PG.Query(`SELECT summary FROM admin_audit_log WHERE category = 'docs' AND action = $1 AND metadata->>'resource_uuid' = $2 ORDER BY seq`, action, doc)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	mention := func(doc string, who ...string) {
		t.Helper()
		var b strings.Builder
		b.WriteString("<p>Notes for ")
		for _, n := range who {
			fmt.Fprintf(&b, `<span data-type="mention" data-id="%s@%s" data-label="%s">@%s</span> `, people[n], uids[n], n, n)
		}
		b.WriteString("</p>")
		if _, err := docBusiness.SaveFromCollab(ctx, docBusiness.CollabSave{DocUUID: doc, HTML: b.String(), Contributors: []string{people["olu"].String()}}); err != nil {
			t.Fatal(err)
		}
		integration.Settle(t)
	}
	asked := func(doc, about string) bool {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM resource_notices WHERE user_uuid = $1 AND kind = 'doc.mention.unshared' AND resource_uuid = $2 AND data->>'person' = $3`,
			people["olu"], doc, people[about].String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}

	plan := create("olu", "Plan", nil)
	notes := create("olu", "Project notes", map[string]any{"space": "project:" + project})
	// Cy's view of Plan ends a moment after it's given.
	var cyUntil time.Time

	t.Run("doc.shared says the level each person was given", func(t *testing.T) {
		cyUntil = time.Now().Add(3 * time.Second).UTC()
		update("olu", map[string]any{"doc_uuid": plan, "adds": []any{
			user("ann", "full"), user("bea", "edit"),
			map[string]any{"kind": "team", "uuid": design, "level": "view"},
			map[string]any{"kind": "user", "uuid": people["cy"].String(), "level": "view", "until": cyUntil.Format(time.RFC3339Nano)},
			map[string]any{"kind": "user", "uuid": people["di"].String(), "level": "view", "until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)},
		}})
		integration.Settle(t)
		for who, want := range map[string]string{"ann": "full", "bea": "edit", "eve": "view", "fay": "view", "cy": "view", "di": "view"} {
			var lvl string
			if err := env.PG.QueryRow(`SELECT COALESCE(data->>'level', '') FROM resource_notices WHERE user_uuid = $1 AND kind = 'doc.shared' AND resource_uuid = $2`,
				people[who], plan).Scan(&lvl); err != nil || lvl != want {
				t.Errorf("%s's doc.shared notice says %q (%v), want %q", who, lvl, err, want)
			}
		}
		if got := pushedTo("bea"); len(got) != 1 || got[0].body != "You can edit it." {
			t.Errorf("bea was pushed %+v", got)
		}
		rows := audited("doc.shared", plan)
		slices.Sort(rows)
		want := []string{"Shared a private page with 1 person who can edit", "Shared a private page with 1 person who has full access", "Shared a private page with 4 people who can view"}
		if !slices.Equal(rows, want) {
			t.Errorf("doc.shared rows %q, want %q", rows, want)
		}
	})

	t.Run("a mention reaches someone the doc reaches through a team, not someone it doesn't", func(t *testing.T) {
		mention(plan, "eve", "zed")
		if notices("eve", "doc.mentioned", plan) != 1 || asked(plan, "eve") {
			t.Errorf("eve, in the team the doc is shared with: %d mention notices, asked to share %v", notices("eve", "doc.mentioned", plan), asked(plan, "eve"))
		}
		if notices("zed", "doc.mentioned", plan) != 0 || !asked(plan, "zed") {
			t.Errorf("zed, who can't read it: %d mention notices, olu asked to share %v", notices("zed", "doc.mentioned", plan), asked(plan, "zed"))
		}
	})

	t.Run("a grant that has ended is no reader, before the sweep and after it", func(t *testing.T) {
		time.Sleep(time.Until(cyUntil.Add(500 * time.Millisecond)))
		// The sweep hasn't run: the graph still lists Cy, but the grant's end
		// has passed. Di's ends in an hour.
		mention(plan, "cy", "di")
		if notices("cy", "doc.mentioned", plan) != 0 || !asked(plan, "cy") {
			t.Errorf("cy, whose grant ended: %d mention notices, olu asked to share %v", notices("cy", "doc.mentioned", plan), asked(plan, "cy"))
		}
		if notices("di", "doc.mentioned", plan) != 1 || asked(plan, "di") {
			t.Errorf("di, whose grant runs on: %d mention notices", notices("di", "doc.mentioned", plan))
		}
		can, err := docBusiness.Readers(ctx, plan, []string{people["cy"].String(), people["di"].String()})
		if err != nil || can[people["cy"].String()] || !can[people["di"].String()] {
			t.Errorf("readers before the sweep: %v %v", can, err)
		}
		// Cy's own side agrees, before the sweep: the resolver and opening the doc.
		if got := levelOf("cy", plan); got != "none" {
			t.Errorf("before the sweep the resolver gives cy %s", got)
		}
		if a := call("cy", "GET", "/doc/getDocInfo/"+plan, docController.GetDocPage, nil, map[string]string{"doc_uuid": plan}); a.code != 403 {
			t.Errorf("before the sweep cy opened the doc: %d", a.code)
		}
		if got := levelOf("di", plan); got != "view" {
			t.Errorf("di, whose grant runs on, has %s", got)
		}
		if err := accessBusiness.EndDueGrants(ctx); err != nil {
			t.Fatal(err)
		}
		integration.Settle(t)
		if got := levelOf("cy", plan); got != "none" {
			t.Errorf("after the sweep cy has %s", got)
		}
		if can, _ := docBusiness.Readers(ctx, plan, []string{people["cy"].String()}); can[people["cy"].String()] {
			t.Error("after the sweep cy still reads it")
		}
	})

	t.Run("someone taken out of the doc's space is no reader; someone still in it is", func(t *testing.T) {
		if got := levelOf("hal", notes); got != "view" {
			t.Fatalf("hal, in P, has %s on P's notes", got)
		}
		if _, err := dgraphInit.DgraphClient.NewTxn().Mutate(ctx, &api.Mutation{
			DelNquads: []byte(fmt.Sprintf("<%s> <project_members> <%s> .", uids["p"], uids["hal"])), CommitNow: true,
		}); err != nil {
			t.Fatal(err)
		}
		mention(notes, "gus", "hal")
		if notices("gus", "doc.mentioned", notes) != 1 || asked(notes, "gus") {
			t.Errorf("gus, in P: %d mention notices", notices("gus", "doc.mentioned", notes))
		}
		if notices("hal", "doc.mentioned", notes) != 0 || !asked(notes, "hal") {
			t.Errorf("hal, taken out of P: %d mention notices, olu asked to share %v", notices("hal", "doc.mentioned", notes), asked(notes, "hal"))
		}
	})

	t.Run("the doc's side and each person's side agree, both ways", func(t *testing.T) {
		var all []string
		for _, n := range names {
			all = append(all, people[n].String())
		}
		for _, doc := range []string{plan, notes} {
			can, err := docBusiness.Readers(ctx, doc, all)
			if err != nil {
				t.Fatal(err)
			}
			readers := 0
			for _, n := range names {
				person := levelOf(n, doc) != "none"
				if can[people[n].String()] != person {
					t.Errorf("%s on %s: the doc's side says %v, theirs %v", n, doc, can[people[n].String()], person)
				}
				if person {
					readers++
				}
			}
			if readers == 0 || readers == len(names) {
				t.Errorf("%s: %d readers of %d; the check needs both", doc, readers, len(names))
			}
		}
	})

	var declined string
	t.Run("a request reaches the people who can answer it", func(t *testing.T) {
		a := call("zed", "POST", "/doc/access/request", docController.RequestDocAccess,
			map[string]any{"doc_uuid": plan, "level": "edit", "note": "For the launch review"}, nil)
		if a.code != 200 {
			t.Fatalf("zed asking: %d %v", a.code, a.body)
		}
		declined = str(a.data()["id"])
		integration.Settle(t)
		for who, want := range map[string]int{"olu": 1, "ann": 1, "bea": 0, "eve": 0, "zed": 0} {
			if got := notices(who, "doc.access_requested", plan); got != want {
				t.Errorf("%s has %d request notices, want %d", who, got, want)
			}
		}
		if got := pushedTo("ann"); len(got) == 0 || got[len(got)-1].title != "zed asked to edit Plan" || !strings.Contains(got[len(got)-1].body, "For the launch review") {
			t.Errorf("ann was pushed %+v", got)
		}
		if rows := audited("doc.access_requested", plan); !slices.Equal(rows, []string{"Asked to edit a private page"}) {
			t.Errorf("request rows %q", rows)
		}
	})

	t.Run("a declined asker hears it, and nothing about the doc", func(t *testing.T) {
		before := len(pushedTo("zed"))
		if a := call("olu", "POST", "/doc/access/answer", docController.AnswerDocAccess, map[string]any{"request_id": declined, "approve": false}, nil); a.code != 200 {
			t.Fatalf("declining: %d %v", a.code, a.body)
		}
		integration.Settle(t)
		if notices("zed", "doc.access_answered", plan) != 1 {
			t.Fatalf("zed has %d answer notices", notices("zed", "doc.access_answered", plan))
		}
		// The row: nobody named, ids and words of the request only.
		var actor, agent string
		var raw []byte
		if err := env.PG.QueryRow(`SELECT COALESCE(actor_uuid::text, ''), COALESCE(agent_uuid::text, ''), data FROM resource_notices
			WHERE user_uuid = $1 AND kind = 'doc.access_answered' AND resource_uuid = $2`, people["zed"], plan).Scan(&actor, &agent, &raw); err != nil {
			t.Fatal(err)
		}
		var data map[string]string
		_ = json.Unmarshal(raw, &data)
		if actor != "" || agent != "" || len(data) != 3 || data["request"] != declined || data["status"] != "declined" || data["level"] != "edit" {
			t.Errorf("the declined notice's row: actor %q agent %q data %v", actor, agent, data)
		}
		// The push.
		got := pushedTo("zed")[before:]
		if len(got) != 1 || got[0].title != "Your request for access wasn't approved" {
			t.Errorf("zed was pushed %+v", got)
		}
		for _, p := range got {
			for _, s := range []string{p.title, p.body} {
				if strings.Contains(s, "Plan") || strings.Contains(s, "olu") {
					t.Errorf("the push to zed names the doc or who declined: %q", s)
				}
			}
		}
		// The inbox, as zed reads it.
		page := activityBusiness.WithNotices(ctx, dgraphModels.UnifiedActivityPagination{}, people["zed"].String(), time.Now().Add(time.Second), 20)
		var found *dgraphModels.NoticeItem
		for _, it := range page.Activities {
			if it.Notice != nil && it.Notice.Kind == "doc.access_answered" {
				found = it.Notice
			}
		}
		if found == nil || found.Title != "" || found.CanOpen || found.Actor != nil || found.Agent != nil {
			t.Errorf("zed's inbox item: %+v", found)
		}
		if rows := audited("doc.access_answered", plan); !slices.Equal(rows, []string{"Declined a request to edit a private page"}) {
			t.Errorf("answer rows %q", rows)
		}
	})

	t.Run("an approved asker hears who approved it, once", func(t *testing.T) {
		a := call("zed", "POST", "/doc/access/request", docController.RequestDocAccess, map[string]any{"doc_uuid": plan, "level": "edit"}, nil)
		if a.code != 200 {
			t.Fatalf("zed asking again: %d %v", a.code, a.body)
		}
		integration.Settle(t)
		if notices("olu", "doc.access_requested", plan) != 2 {
			t.Errorf("olu has %d request notices after a second request", notices("olu", "doc.access_requested", plan))
		}
		before := len(pushedTo("zed"))
		if a := call("olu", "POST", "/doc/access/answer", docController.AnswerDocAccess,
			map[string]any{"request_id": str(a.data()["id"]), "approve": true, "level": "comment"}, nil); a.code != 200 {
			t.Fatalf("approving: %d %v", a.code, a.body)
		}
		integration.Settle(t)
		if got := levelOf("zed", plan); got != "comment" {
			t.Errorf("zed has %s after the approval", got)
		}
		if notices("zed", "doc.access_answered", plan) != 2 || notices("zed", "doc.shared", plan) != 0 {
			t.Errorf("zed has %d answer and %d shared notices, want 2 and none: the approval's share is the answer's to tell",
				notices("zed", "doc.access_answered", plan), notices("zed", "doc.shared", plan))
		}
		if got := pushedTo("zed")[before:]; len(got) != 1 || got[0].title != "olu approved your request for ‘Plan’" || got[0].body != "You can read it and comment on it." {
			t.Errorf("zed was pushed %+v", got)
		}
		if rows := audited("doc.access_answered", plan); len(rows) != 2 || rows[1] != "Approved a request to edit a private page, with comment access" {
			t.Errorf("answer rows %q", rows)
		}
	})

	t.Run("the old updateDocPermissions handler goes by A5's rules", func(t *testing.T) {
		old := func(who string, body map[string]any) answer {
			body["doc_uuid"] = plan
			return call(who, "POST", "/doc/updateDocPermissions", docController.UpdateDocPermissions, body, nil)
		}
		// Full access may share: the owner alone could, through this handler.
		if a := old("ann", map[string]any{"add_editors": []string{people["ivy"].String()}}); a.code != 200 || levelOf("ivy", plan) != "edit" {
			t.Errorf("ann, with full access, sharing: %d %v; ivy has %s", a.code, a.body, levelOf("ivy", plan))
		}
		// An editor may not.
		if a := old("bea", map[string]any{"add_viewers": []string{people["hal"].String()}}); a.code != 403 || levelOf("hal", plan) != "none" {
			t.Errorf("bea, an editor, sharing: %d %v", a.code, a.body)
		}
		// Shared again for good, Di's grant no longer ends.
		ends := func() []*accessModels.Expiry {
			rows, err := accessModels.ListExpiry(ctx, "doc", uuid.MustParse(plan))
			if err != nil {
				t.Fatal(err)
			}
			return rows
		}
		if e := ends(); len(e) != 1 || e[0].PrincipalUUID != people["di"] {
			t.Fatalf("grants that end before: %+v", e)
		}
		if a := old("olu", map[string]any{"add_viewers": []string{people["di"].String()}}); a.code != 200 || len(ends()) != 0 {
			t.Errorf("sharing with di for good: %d %v; ends left %+v", a.code, a.body, ends())
		}
		// Taking Ivy off is A5's change: recorded, with Ivy's sockets asked again.
		if a := old("olu", map[string]any{"remove_editors": []string{people["ivy"].String()}}); a.code != 200 || levelOf("ivy", plan) != "none" {
			t.Errorf("taking ivy off: %d %v", a.code, a.body)
		}
		integration.Settle(t)
		if rows := audited("doc.access_changed", plan); len(rows) == 0 {
			t.Error("no doc.access_changed rows for the changes through the old handler")
		}
		// The demo keeps its own docs from its shared visitor here too.
		t.Setenv("DEMO_MODE", "true")
		t.Setenv("DEMO_USER_EMAIL", people["olu"].String()+"@example.test")
		if err := demoGuard.MarkSeeded(time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if a := old("olu", map[string]any{"add_viewers": []string{people["hal"].String()}}); a.code != 403 || a.body["code"] != "demo" || levelOf("hal", plan) != "none" {
			t.Errorf("the demo's visitor sharing a demo doc: %d %v", a.code, a.body)
		}
	})
}
