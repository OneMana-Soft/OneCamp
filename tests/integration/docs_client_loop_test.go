//go:build integration
// +build integration

package integration_test

// Docs C5-2 against real Postgres 12 and Dgraph: the migration renames the
// task reviews in place and keeps them, goes down exactly and up again; a
// member answers a guest where the guest sees it (the guest's own limits, a
// name never an address) while members' comments stay out of the guest's
// view; whoever may moderate removes a guest's comment, recorded; one guest is
// one author; a comment link's client gives the doc a verdict.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestDocsClientLoop' -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ClientReview"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func clientLoopMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_docs_client_loop."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one docs_client_loop %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDocsClientLoopMigration(t *testing.T) {
	up, down := clientLoopMigration(t, "up"), clientLoopMigration(t, "down")
	if strings.Contains(up, "gen_random_uuid") || strings.Contains(up, "INSERT INTO client_reviews") {
		t.Fatal("the migration copies rows or uses gen_random_uuid")
	}
	env := integration.SetupEnv(t)
	user := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'u', 'u', NOW(), NOW())`, user, user.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	grant := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO guest_grants (id, token_hash, resource_type, resource_id, capability, created_by) VALUES ($1, $2, 'project', $3, 'comment', $4)`,
		grant, []byte(grant.String()), uuid.NewString(), user); err != nil {
		t.Fatal(err)
	}
	// A task review kept before the migration, as live installs hold them.
	if _, err := env.PG.Exec(down); err != nil {
		t.Fatalf("down to before: %v", err)
	}
	task, project := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO task_client_reviews (task_uuid, project_uuid, grant_id, decision, name) VALUES ($1, $2, $3, 'approved', 'Dana')`, task, project, grant); err != nil {
		t.Fatal(err)
	}
	for i, step := range []string{up, down, up} {
		if _, err := env.PG.Exec(step); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	var kind string
	if err := env.PG.QueryRow(`SELECT resource_kind FROM client_reviews WHERE task_uuid = $1`, task).Scan(&kind); err != nil || kind != "task" {
		t.Fatalf("the task review after the rename: %q %v", kind, err)
	}
	if _, err := env.PG.Exec(`INSERT INTO client_reviews (resource_kind, doc_uuid, task_uuid, project_uuid, grant_id, decision, name) VALUES ('doc', 'd', $1, $2, $3, 'approved', 'x')`, task, project, grant); err == nil {
		t.Error("a doc review that names a task was kept")
	}
	if _, err := env.PG.Exec(`INSERT INTO guest_comments (grant_id, doc_uuid, guest_name, body, author_kind) VALUES ($1, 'd', 'x', 'y', 'member')`, grant); err == nil {
		t.Error("a member's reply with no member was kept")
	}
}

func TestDocsClientLoopEndToEnd(t *testing.T) {
	ctx := context.Background()
	t.Setenv("GUEST_ACCESS_ENABLED", "")
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	settingsBusiness.ForgetSettingsForTest()
	t.Cleanup(settingsBusiness.ForgetSettingsForTest)
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	osClient, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = osClient

	// Olu owns the doc; Rea only reads it; an outsider has nothing.
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, name := range []string{"olu", "rea", "out"} {
		id := uuid.New()
		people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			id, name+"@acme.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_full_name": strings.ToUpper(name[:1]) + name[1:]})
	}
	doc := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	nodes = append(nodes, map[string]any{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "Proposal", "doc_private": true,
		"doc_body": "<p>Scope</p>", "doc_deleted_at": "0001-01-01T00:00:00Z", "doc_created_at": now, "doc_updated_at": now,
		"doc_created_by": map[string]any{"uid": "_:olu"}, "doc_owner": map[string]any{"uid": "_:olu"},
		"doc_reading_users": []map[string]any{{"uid": "_:rea"}}})
	dg := integration.SetupDgraph(t)
	uids := dg.Mutate(t, nodes)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	userOf := func(name string) *userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name]
		u.UserPostgresInfo.EmailID = name + "@acme.test"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: people[name].String(), UserName: name, UserFullName: strings.ToUpper(name[:1]) + name[1:]}
		return &u
	}
	h := chi.NewRouter()
	h.Get("/guest/doc-comments/{token}", guestController.GuestDocComments)
	h.Post("/guest/doc-comments/{token}", guestController.CreateGuestDocComment)
	h.Post("/guest/doc-review/{token}", guestController.ReviewGuestDoc)
	h.Get("/doc/guest-thread", guestController.GuestThread)
	h.Post("/doc/guest-thread/reply", guestController.GuestThreadReply)
	h.Post("/doc/guest-thread/remove", guestController.GuestThreadRemove)
	h.Get("/doc/client-review", guestController.DocClientReview)
	call := func(method, target, body string, who *userModels.UserInfo) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.RemoteAddr = "203.0.113.9:40000"
		if who != nil {
			r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, *who))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	type item struct {
		ID       string `json:"id"`
		Name     string `json:"guest_name"`
		Body     string `json:"body"`
		ParentID string `json:"parent_id"`
		FromTeam bool   `json:"from_team"`
		AuthorID string `json:"author_id"`
	}
	guestView := func(token string) []item {
		w := call(http.MethodGet, "/guest/doc-comments/"+token, "", nil)
		var out struct {
			Data struct {
				Comments []item `json:"comments"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out.Data.Comments
	}

	link, err := guestBusiness.CreateResourceGrant(ctx, people["olu"], "doc", doc, "comment", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	viewLink, err := guestBusiness.CreateResourceGrant(ctx, people["olu"], "doc", doc, "view", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Is section 2 final?", "Also the dates."} {
		if w := call(http.MethodPost, "/guest/doc-comments/"+link.Token, `{"display_name":"Dana","body":"`+text+`"}`, nil); w.Code != http.StatusOK {
			t.Fatalf("a guest's comment: %d %s", w.Code, w.Body)
		}
	}
	first := guestView(link.Token)
	if len(first) != 2 || first[0].AuthorID != first[1].AuthorID || first[0].AuthorID == "" {
		t.Fatalf("one guest, one author: %+v", first)
	}

	// Olu answers, where Dana sees it; markup goes, and no address shows.
	reply := call(http.MethodPost, "/doc/guest-thread/reply", `{"doc_uuid":"`+doc+`","parent_id":"`+first[0].ID+`","body":"<b>Yes</b>, final."}`, userOf("olu"))
	if reply.Code != http.StatusOK {
		t.Fatalf("Olu's reply: %d %s", reply.Code, reply.Body)
	}
	seen := guestView(link.Token)
	var answer *item
	for i := range seen {
		if seen[i].FromTeam {
			answer = &seen[i]
		}
	}
	if answer == nil || answer.ParentID != first[0].ID || answer.Body != "Yes, final." || strings.Contains(answer.Name, "@") || !strings.HasPrefix(answer.Name, "Olu") {
		t.Fatalf("the reply as Dana sees it: %+v", seen)
	}
	// Rea only reads: no replies, no removing.
	if w := call(http.MethodPost, "/doc/guest-thread/reply", `{"doc_uuid":"`+doc+`","parent_id":"`+first[1].ID+`","body":"hi"}`, userOf("rea")); w.Code != http.StatusForbidden {
		t.Errorf("a reader replied: %d %s", w.Code, w.Body)
	}
	if w := call(http.MethodPost, "/doc/guest-thread/remove", `{"doc_uuid":"`+doc+`","comment_id":"`+first[1].ID+`"}`, userOf("rea")); w.Code != http.StatusForbidden {
		t.Errorf("a reader removed a guest's comment: %d %s", w.Code, w.Body)
	}
	if w := call(http.MethodGet, "/doc/guest-thread?doc_uuid="+doc, "", userOf("out")); w.Code != http.StatusNotFound {
		t.Errorf("an outsider read the guest conversation: %d", w.Code)
	}
	// The owner removes one; it goes from Dana's page at once, recorded.
	if w := call(http.MethodPost, "/doc/guest-thread/remove", `{"doc_uuid":"`+doc+`","comment_id":"`+first[1].ID+`"}`, userOf("olu")); w.Code != http.StatusOK {
		t.Fatalf("the owner's removal: %d %s", w.Code, w.Body)
	}
	if after := guestView(link.Token); len(after) != 2 {
		t.Errorf("after the removal Dana sees %d comments, want her first and its reply", len(after))
	}

	// A verdict: never through a view link; through the comment link, kept,
	// said in the conversation, and shown to the team.
	if w := call(http.MethodPost, "/guest/doc-review/"+viewLink.Token, `{"display_name":"Dana","decision":"approved"}`, nil); w.Code != http.StatusForbidden {
		t.Errorf("a view link gave a verdict: %d %s", w.Code, w.Body)
	}
	if w := call(http.MethodPost, "/guest/doc-review/"+link.Token, `{"display_name":"Dana","decision":"changes","note":"Move the launch to May"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("a verdict: %d %s", w.Code, w.Body)
	}
	if r, err := reviewModel.LatestForDoc(doc); err != nil || r == nil || r.Decision != reviewModel.Changes || r.Note != "Move the launch to May" {
		t.Fatalf("the doc's verdict: %+v %v", r, err)
	}
	if w := call(http.MethodGet, "/doc/client-review?doc_uuid="+doc, "", userOf("rea")); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Move the launch to May") {
		t.Errorf("the team's view of the verdict: %d %s", w.Code, w.Body)
	}
	// Task reviews still work beside it.
	task, project := uuid.New(), uuid.New()
	if _, err := reviewModel.Add(task, project, link.GrantID, reviewModel.Approved, "Dana", ""); err != nil {
		t.Fatalf("a task review after the rename: %v", err)
	}
	if got, err := reviewModel.LatestForProject(project); err != nil || got[task].Decision != reviewModel.Approved {
		t.Errorf("task reviews after the rename: %+v %v", got, err)
	}

	// Each removal is recorded.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		_ = env.PG.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE action = 'doc.guest_comment.remove' AND metadata->>'comment_id' = $1`, first[1].ID).Scan(&n)
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Error("no audit row for the removal")
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	integration.Settle(t)
}
