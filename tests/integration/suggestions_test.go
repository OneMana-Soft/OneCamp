//go:build integration
// +build integration

package integration_test

// A person's suggestion on a doc (docs C5), through the route, against a real
// Postgres and graph: someone who may only comment suggests a change; the
// doc's list names who suggested each, and never by their address; a reader
// who can't comment can't suggest; a hard lock takes no suggestions; the
// owner declines it, and the list says so. And who may act on one: someone
// the doc isn't shared with is told it isn't there and nothing is kept, and
// the demo's shared visitor and an API token act only within their level.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAPersonSuggestsAChange|TestWhoMayActOnASuggestion' -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/business/Access/level"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	proposalController "github.com/akashc777/OneCamp/controllers/Proposal"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// suggestionDoc is a private doc its owner shares with people by name, and
// the suggestion routes called as the router calls them once someone is
// signed in.
type suggestionDoc struct {
	t       *testing.T
	pg      *sql.DB
	docUUID string
	ids     map[string]uuid.UUID
	uids    map[string]string
	asOwner accessBusiness.Subject
}

// newSuggestionDoc makes the people, by name with their display names as they
// set them ("" for none), and "owner"'s private doc. Each has the address
// <name>@acme.test.
func newSuggestionDoc(t *testing.T, people map[string]string) *suggestionDoc {
	t.Helper()
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	d := &suggestionDoc{t: t, pg: env.PG, ids: map[string]uuid.UUID{}}
	var nodes []map[string]any
	for name, display := range people {
		d.ids[name] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, NULLIF($4, ''), NOW(), NOW())`,
			d.ids[name], name+"@acme.test", name, display); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": d.ids[name].String(), "user_name": name})
	}
	d.uids = dg.Mutate(t, nodes)
	priv := true
	owner := d.info("owner")
	doc, err := docBusiness.CreateDocInTree(ctx, &owner.UserDgraphInfo, docBusiness.NewDoc{Title: "Launch plan", Private: &priv}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.docUUID = doc.Uuid
	d.asOwner = accessBusiness.SubjectOf(&owner)
	return d
}

func (d *suggestionDoc) info(name string) userModels.UserInfo {
	return userModels.UserInfo{
		UserPostgresInfo: userModels.User{EmailID: name + "@acme.test"},
		UserDgraphInfo:   dgraphStruct.DgraphUser{Uid: d.uids[name], Uuid: d.ids[name].String(), UserName: name},
	}
}

func (d *suggestionDoc) share(name string, lv accessBusiness.Level) {
	d.t.Helper()
	if _, err := accessBusiness.Apply(context.Background(), d.asOwner, accessBusiness.Change{
		Resource: accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: d.docUUID},
		Add:      []accessBusiness.Grant{{Kind: accessBusiness.PrincipalUser, UUID: d.ids[name].String(), Level: lv}},
	}); err != nil {
		d.t.Fatal(err)
	}
}

// signedIn is a request's context once the auth middleware has put name on it.
func (d *suggestionDoc) signedIn(name string) context.Context {
	return context.WithValue(context.Background(), helpers.UserInfoContextKey, d.info(name))
}

// byToken is that context when one of name's API tokens made the call, as the
// token middleware leaves it.
func (d *suggestionDoc) byToken(name string) context.Context {
	return context.WithValue(d.signedIn(name), helpers.ApiTokenIDContextKey, uuid.NewString())
}

func (d *suggestionDoc) call(c context.Context, method, path, body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	d.t.Helper()
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(c))
	return rec
}

// suggest sends the change the browser sends: "Thursday" to "Friday".
func (d *suggestionDoc) suggest(c context.Context) *httptest.ResponseRecorder {
	d.t.Helper()
	return d.call(c, http.MethodPost, "/proposals", `{"resource_kind":"doc","resource_uuid":"`+d.docUUID+`","anchor":{"kind":"none","quote":"Ship on Thursday."},"base_hash":"h",
		"base":[{"type":"paragraph"}],"proposed":[{"type":"paragraph"}],
		"hunks":[{"id":"h1","op":"replace","level":"inline","path":[0],"from":8,"to":16,"before":[{"type":"text","text":"Thursday"}],"after":[{"type":"text","text":"Friday"}]}]}`,
		proposalController.CreateProposal)
}

// on calls one of a suggestion's routes (decide, withdraw) as the router does.
func (d *suggestionDoc) on(c context.Context, id, action, body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	d.t.Helper()
	return d.call(c, http.MethodPost, "/proposals/"+id+"/"+action, body, withID(id, handler))
}

// status is a suggestion's status, as its owner reads it.
func (d *suggestionDoc) status(id string) string {
	d.t.Helper()
	rec := d.call(d.signedIn("owner"), http.MethodGet, "/proposals/"+id, "", withID(id, proposalController.GetProposal))
	var got struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		d.t.Fatalf("reading suggestion %s: %d %s", id, rec.Code, rec.Body)
	}
	return got.Data.Status
}

// kept is how many suggestions by name the doc holds, whatever their status.
func (d *suggestionDoc) kept(name string) int {
	d.t.Helper()
	var n int
	if err := d.pg.QueryRow(`SELECT count(*) FROM edit_proposals WHERE resource_uuid = $1 AND author_uuid = $2`, d.docUUID, d.ids[name]).Scan(&n); err != nil {
		d.t.Fatal(err)
	}
	return n
}

// idOf is the id in a created suggestion's answer.
func idOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var got struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Data.ID == "" {
		t.Fatalf("no id in %s", rec.Body)
	}
	return got.Data.ID
}

func TestAPersonSuggestsAChange(t *testing.T) {
	ctx := context.Background()
	d := newSuggestionDoc(t, map[string]string{"owner": "Olu Owner", "maya": "Maya Patel", "nameless": "", "reader": "Rae Reader"})
	d.share("maya", accessBusiness.LevelComment)
	d.share("nameless", accessBusiness.LevelComment)
	d.share("reader", accessBusiness.LevelView)

	// Someone who may only comment suggests; so does someone with no name set.
	for _, who := range []string{"maya", "nameless"} {
		if rec := d.suggest(d.signedIn(who)); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("%s suggesting: %d %s", who, rec.Code, rec.Body)
		}
	}
	// A reader who can't comment can't.
	if rec := d.suggest(d.signedIn("reader")); rec.Code != http.StatusForbidden {
		t.Errorf("a reader suggesting: %d %s", rec.Code, rec.Body)
	}

	list := func(as string) []map[string]any {
		t.Helper()
		rec := d.call(d.signedIn(as), http.MethodGet, "/proposals?resource_kind=doc&resource_uuid="+d.docUUID, "", proposalController.ListProposals)
		if rec.Code != http.StatusOK {
			t.Fatalf("the list as %s: %d %s", as, rec.Code, rec.Body)
		}
		var got struct {
			Data struct {
				Proposals []map[string]any `json:"proposals"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.Data.Proposals
	}
	byAuthor := map[string]map[string]any{}
	for _, p := range list("owner") {
		byAuthor[p["author_uuid"].(string)] = p
	}
	maya, nameless := byAuthor[d.ids["maya"].String()], byAuthor[d.ids["nameless"].String()]
	if maya == nil || maya["author_name"] != "Maya Patel" || maya["author_kind"] != "person" || maya["status"] != "open" {
		t.Errorf("Maya's suggestion: %+v", maya)
	}
	if nameless == nil {
		t.Fatal("the nameless person's suggestion isn't listed")
	}
	if name, has := nameless["author_name"]; has {
		t.Errorf("someone with no name set is listed as %q", name)
	}
	raw, _ := json.Marshal(list("maya"))
	if strings.Contains(string(raw), "@acme.test") {
		t.Errorf("the list shows an address: %s", raw)
	}

	// The owner declines Maya's; the list says so.
	id := maya["id"].(string)
	if rec := d.on(d.signedIn("owner"), id, "decide", `{"accept":false}`, proposalController.DecideProposal); rec.Code != http.StatusOK {
		t.Fatalf("declining: %d %s", rec.Code, rec.Body)
	}
	for _, p := range list("owner") {
		if p["id"] == id && p["status"] != "declined" {
			t.Errorf("declined, the list says %v", p["status"])
		}
	}

	// A hard lock takes no suggestions, from anyone who may comment.
	if _, err := accessBusiness.Lock(ctx, d.asOwner, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: d.docUUID}, level.LockHard); err != nil {
		t.Fatal(err)
	}
	if rec := d.suggest(d.signedIn("maya")); rec.Code != http.StatusLocked {
		t.Errorf("suggesting on a hard-locked doc: %d %s", rec.Code, rec.Body)
	}
}

// Who may act on a suggestion (the owner's rule for every head, 25 Oct): each
// caller here is refused by the level it holds on the doc, and a refused call
// changes nothing. An API token acts as its member and never more; the demo's
// shared visitor is a member like any other, at the level the doc gives it.
func TestWhoMayActOnASuggestion(t *testing.T) {
	// The public demo, where everyone who opens it is one shared visitor.
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@acme.test")
	d := newSuggestionDoc(t, map[string]string{
		"owner": "Olu Owner", "maya": "Maya Patel", "cy": "Cy Okafor", "rae": "Rae Reader", "visitor": "Demo Visitor", "stranger": "Sam Stranger", "ed": "Ed Editor",
	})
	d.share("ed", accessBusiness.LevelEdit)
	d.share("maya", accessBusiness.LevelComment)
	d.share("cy", accessBusiness.LevelComment)
	d.share("rae", accessBusiness.LevelView)
	d.share("visitor", accessBusiness.LevelView)

	// Maya's suggestion, open, for the others to try to act on.
	rec := d.suggest(d.signedIn("maya"))
	if rec.Code != http.StatusOK {
		t.Fatalf("Maya suggesting: %d %s", rec.Code, rec.Body)
	}
	mayas := idOf(t, rec)

	t.Run("someone the doc isn't shared with is told it isn't there, and nothing is kept", func(t *testing.T) {
		if rec := d.suggest(d.signedIn("stranger")); rec.Code != http.StatusNotFound {
			t.Errorf("a stranger suggesting: %d %s", rec.Code, rec.Body)
		}
		if n := d.kept("stranger"); n != 0 {
			t.Errorf("the stranger's refused suggestion was kept (%d)", n)
		}
	})

	// The list the doc reads as it opens (GET /proposals, the web's
	// useDocProposals): whoever may open the doc reads its suggestions, a
	// reader and the demo's visitor too; someone it isn't shared with is told
	// it isn't there, and learns nothing of what was suggested.
	t.Run("everyone the doc is shared with reads its suggestions, and a stranger is told it isn't there", func(t *testing.T) {
		list := func(as string) *httptest.ResponseRecorder {
			return d.call(d.signedIn(as), http.MethodGet, "/proposals?resource_kind=doc&resource_uuid="+d.docUUID, "", proposalController.ListProposals)
		}
		for _, who := range []string{"rae", "visitor", "cy", "ed"} {
			if rec := list(who); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), mayas) {
				t.Errorf("%s reading the suggestions: %d %s", who, rec.Code, rec.Body)
			}
		}
		if rec := list("stranger"); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), mayas) {
			t.Errorf("a stranger reading the suggestions: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("the demo's shared visitor, who may only read, can't suggest, accept or decline", func(t *testing.T) {
		if !helpers.IsDemoVisitor(d.info("visitor").UserPostgresInfo.EmailID) {
			t.Fatal("the visitor isn't the demo's shared visitor")
		}
		if rec := d.suggest(d.signedIn("visitor")); rec.Code != http.StatusForbidden {
			t.Errorf("the visitor suggesting: %d %s", rec.Code, rec.Body)
		}
		if n := d.kept("visitor"); n != 0 {
			t.Errorf("the visitor's refused suggestion was kept (%d)", n)
		}
		for _, body := range []string{`{"accept":true}`, `{"accept":false}`} {
			if rec := d.on(d.signedIn("visitor"), mayas, "decide", body, proposalController.DecideProposal); rec.Code != http.StatusForbidden {
				t.Errorf("the visitor deciding %s on Maya's: %d %s", body, rec.Code, rec.Body)
			}
		}
		if s := d.status(mayas); s != "open" {
			t.Errorf("after the visitor's refused decisions, Maya's is %q", s)
		}
	})

	t.Run("an API token acting for someone who may comment suggests, and can't accept", func(t *testing.T) {
		rec := d.suggest(d.byToken("cy"))
		if rec.Code != http.StatusOK {
			t.Fatalf("Cy's token suggesting: %d %s", rec.Code, rec.Body)
		}
		cys := idOf(t, rec)
		// Neither its own suggestion, which would let a commenter edit, nor Maya's.
		for _, id := range []string{cys, mayas} {
			if rec := d.on(d.byToken("cy"), id, "decide", `{"accept":true}`, proposalController.DecideProposal); rec.Code != http.StatusForbidden {
				t.Errorf("Cy's token accepting %s: %d %s", id, rec.Code, rec.Body)
			}
			if s := d.status(id); s != "open" {
				t.Errorf("after the token's refused accept, %s is %q", id, s)
			}
		}
	})

	t.Run("an API token acting for someone who may only read can't withdraw another's suggestion", func(t *testing.T) {
		if rec := d.on(d.byToken("rae"), mayas, "withdraw", "", proposalController.WithdrawProposal); rec.Code != http.StatusForbidden {
			t.Errorf("Rae's token withdrawing Maya's: %d %s", rec.Code, rec.Body)
		}
		if s := d.status(mayas); s != "open" {
			t.Errorf("after the token's refused withdraw, Maya's is %q", s)
		}
	})

	t.Run("someone who decided, then was moved down to view, can't reopen the decision", func(t *testing.T) {
		id := idOf(t, d.suggest(d.signedIn("maya")))
		decline := func() {
			t.Helper()
			if rec := d.on(d.signedIn("ed"), id, "decide", `{"accept":false}`, proposalController.DecideProposal); rec.Code != http.StatusOK {
				t.Fatalf("Ed declining: %d %s", rec.Code, rec.Body)
			}
		}
		reopen := func() *httptest.ResponseRecorder {
			return d.on(d.signedIn("ed"), id, "reopen", `{"hunk_ids":["h1"]}`, proposalController.ReopenProposal)
		}
		// While Ed may edit, Ed takes the decision back.
		decline()
		if rec := reopen(); rec.Code != http.StatusOK {
			t.Fatalf("Ed reopening as an editor: %d %s", rec.Code, rec.Body)
		}
		// Moved down to view inside the five minutes, Ed can't, and the decision stands.
		decline()
		d.share("ed", accessBusiness.LevelView)
		if rec := reopen(); rec.Code != http.StatusForbidden {
			t.Errorf("Ed reopening after being moved down to view: %d %s", rec.Code, rec.Body)
		}
		if s := d.status(id); s != "declined" {
			t.Errorf("after Ed's refused reopen, the suggestion is %q", s)
		}
	})
}

// withID gives a handler the proposal's id as the router does (chi's URL
// parameter).
func withID(id string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rc := chi.NewRouteContext()
		rc.URLParams.Add("proposal_id", id)
		h(w, r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc)))
	}
}
