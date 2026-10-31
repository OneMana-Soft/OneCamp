//go:build integration
// +build integration

package integration_test

// Who knows (DG9) credits pages only (docs F1, DGF1), through its handler,
// against real Postgres 12, Dgraph with the production schema and
// OpenSearch mapped as the server maps it: who knows about "budget", as ana
// may see it.
//
//   - hal made a workspace folder, Budget, uploaded budget.xlsx into it and
//     added a link, Budget sheet: three docs ana may open, none of them
//     writing;
//   - jo commented on the upload;
//   - cy wrote a page in the folder, Budget notes;
//   - ivy made a private folder, Budget vault, and wrote a private page in
//     it, Budget salaries: ana can open neither.
//
// Every entry is in the index as a stale copy would have it (all public, the
// upload with an entry of its own), so only the graph's decision and the
// kind keep anything out. Making a folder, uploading a file or adding a link
// isn't writing about the budget, and a comment on an upload isn't either:
// only cy is named, for the page, and only the page is considered.
//
// Run: integ_rel.sh <wt> <log> '^TestWhoKnowsCountsPagesOnly$'

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	whoKnowsController "github.com/akashc777/OneCamp/controllers/WhoKnows"
	whoKnowsDomain "github.com/akashc777/OneCamp/domain/WhoKnows"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestWhoKnowsCountsPagesOnly(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}

	zero := "0001-01-01T00:00:00Z"
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, n := range []string{"ana", "hal", "jo", "cy", "ivy"} {
		people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			people[n], people[n].String()+"@example.test", n, n); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n,
			"user_email_id": people[n].String() + "@example.test", "user_deleted_at": zero})
	}
	id := map[string]string{}
	for _, k := range []string{"folder", "upload", "link", "page", "vault", "salaries", "c1"} {
		id[k] = uuid.NewString()
	}
	ref := func(key string) map[string]any { return map[string]any{"uid": "_:" + key} }
	doc := func(key, title, by string, private bool, extra map[string]any) map[string]any {
		n := map[string]any{"uid": "_:" + key, "dgraph.type": "Doc", "doc_uuid": id[key], "doc_title": title, "doc_private": private,
			"doc_deleted_at": zero, "doc_owner": ref(by), "doc_created_by": ref(by)}
		for k, v := range extra {
			n[k] = v
		}
		return n
	}
	nodes = append(nodes,
		doc("folder", "Budget", "hal", false, map[string]any{"doc_node_kind": "folder", "doc_color": "sun"}),
		doc("upload", "budget.xlsx", "hal", false, map[string]any{"doc_node_kind": "file", "doc_parent": ref("folder"),
			"doc_comments": []map[string]any{ref("c1")}}),
		map[string]any{"uid": "_:c1", "dgraph.type": "Comment", "comment_uuid": id["c1"], "comment_text": "On the budget",
			"comment_deleted_at": zero, "comment_by": ref("jo"), "comment_doc": ref("upload")},
		doc("link", "Budget sheet", "hal", false, map[string]any{"doc_node_kind": "link", "doc_link_url": "https://example.com/budget",
			"doc_parent": ref("folder")}),
		doc("page", "Budget notes", "cy", false, map[string]any{"doc_parent": ref("folder")}),
		doc("vault", "Budget vault", "ivy", true, map[string]any{"doc_node_kind": "folder"}),
		doc("salaries", "Budget salaries", "ivy", true, map[string]any{"doc_parent": ref("vault")}),
	)
	uids := dg.Mutate(t, nodes)

	// The index, as stale as it can be: every entry public, the upload with
	// one of its own (a file has none today: OwnSearchEntry).
	index := func(idx, docID string, body map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest("PUT", fmt.Sprintf("%s/%s/_doc/%s?refresh=true", search.URL, idx, docID), bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("index %s/%s: %d", idx, docID, resp.StatusCode)
		}
	}
	for _, k := range []string{"folder", "upload", "link", "page", "vault", "salaries"} {
		index("docs", id[k], map[string]any{"doc_uuid": id[k], "doc_title": "Budget " + k, "doc_body": "the budget", "doc_private": false})
	}

	name := map[string]string{}
	for k, v := range people {
		name[v.String()] = k
	}
	for k, v := range id {
		name[v] = k
	}
	userOf := func(who string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserPostgresInfo.EmailID = people[who].String() + "@example.test"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[who], Uuid: people[who].String(), UserName: who}
		return u
	}

	t.Run("a folder, an upload and a link ana may open say what they are", func(t *testing.T) {
		got, err := entityLinkBusiness.ReadMany(ctx, accessBusiness.SubjectFor(ctx, ptr(userOf("ana"))),
			[]entityLinkBusiness.Ref{{Kind: "doc", ID: id["folder"]}, {Kind: "doc", ID: id["upload"]}, {Kind: "doc", ID: id["link"]}, {Kind: "doc", ID: id["page"]}})
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range []string{"folder", "file", "link", ""} {
			if got[i].Access != entityLinkBusiness.AccessOK || entityLinkBusiness.NodeKindOf(got[i].Data) != want {
				t.Errorf("%s: %v %v, want open and %q", name[got[i].ID], got[i].Access, got[i].Data, want)
			}
		}
		if got[0].Data["color"] != "sun" {
			t.Errorf("the folder's colour: %v", got[0].Data)
		}
		if _, ok := got[2].Data["link_url"]; ok {
			t.Errorf("the link's chip carries its address: %v", got[2].Data)
		}
	})

	t.Run("only the page credits anyone, and only the page is considered", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/people/who-knows?"+url.Values{"q": {"budget"}}.Encode(), nil)
		rec := httptest.NewRecorder()
		whoKnowsController.GetWhoKnows(rec, req.WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, userOf("ana"))))
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		data, _ := body["data"].(map[string]any)
		var got []string
		ps, _ := data["people"].([]any)
		for _, p := range ps {
			pm := p.(map[string]any)
			var ev []string
			for _, e := range pm["evidence"].([]any) {
				em := e.(map[string]any)
				ev = append(ev, em["role"].(string)+">"+name[em["id"].(string)])
			}
			got = append(got, fmt.Sprintf("%s %v/%v: %s", name[pm["id"].(string)], pm["wrote"], pm["commented"], strings.Join(ev, ",")))
		}
		if strings.Join(got, "\n") != "cy 1/0: wrote>page" {
			t.Errorf("ana hears %v, want cy alone, for the page", got)
		}
		if c, _ := data["considered"].(float64); c != 1 {
			t.Errorf("considered %v, want 1: the page", data["considered"])
		}
		raw := rec.Body.String()
		for _, k := range []string{"hal", "jo", "ivy"} {
			if strings.Contains(raw, people[k].String()) {
				t.Errorf("the answer names %s: %s", k, raw)
			}
		}
		for _, k := range []string{"folder", "upload", "link", "vault", "salaries", "c1"} {
			if strings.Contains(raw, id[k]) {
				t.Errorf("the answer names %s: %s", k, raw)
			}
		}
	})

	t.Run("the count itself takes pages only, whatever it is given", func(t *testing.T) {
		counted, err := whoKnowsDomain.Count(ctx, whoKnowsDomain.Items{Docs: []string{id["folder"], id["upload"], id["link"], id["page"]}})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, p := range counted.People {
			got = append(got, fmt.Sprintf("%s %d/%d", name[p.UUID], p.Wrote, p.Commented))
		}
		if strings.Join(got, ",") != "cy 1/0" {
			t.Errorf("counted %v, want cy alone", got)
		}
		for _, c := range counted.Contributions {
			if c.Item != id["page"] {
				t.Errorf("evidence on %s: %+v", name[c.Item], c)
			}
		}
	})
}
