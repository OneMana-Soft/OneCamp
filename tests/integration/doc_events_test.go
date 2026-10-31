//go:build integration
// +build integration

package integration_test

// The docs programme's events (A6) against real Postgres 12 and Dgraph:
// migration 210 goes up, down and up; a doc's comment, a mention in its body,
// sharing it and following it tell the right people once; every recorded
// action writes one audit row and the chain verifies; and on the free plan the
// log can be read, not exported. Pushes are watched where every path sends
// them, MQTT goes nowhere, and search is a stand-in: only test people are told.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestResourceFollows|TestDocEvents' -v

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	followBusiness "github.com/akashc777/OneCamp/business/Follow"
	lastSeenActivityBusiness "github.com/akashc777/OneCamp/business/LastSeenActivity"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	settingsController "github.com/akashc777/OneCamp/controllers/Settings"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// followsMigration is migration 210's file, found by name so a renumbering at
// merge keeps the test.
func followsMigration(t *testing.T, dir string) (string, string) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_resource_follows."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one resource_follows %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return matches[0], string(b)
}

func TestResourceFollowsMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	upName, up := followsMigration(t, "up")
	_, down := followsMigration(t, "down")
	makesNoIDs(t, upName, up)
	tables := func() int {
		var n int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM information_schema.tables
			WHERE table_name IN ('resource_follows', 'resource_notices', 'resource_mentions')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if tables() != 3 {
		t.Fatal("the migrations did not make the three tables")
	}
	for i, step := range []struct {
		sql  string
		want int
	}{{down, 0}, {up, 3}, {down, 0}, {up, 3}} {
		if _, err := env.PG.Exec(step.sql); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got := tables(); got != step.want {
			t.Fatalf("step %d: %d tables, want %d", i, got, step.want)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO resource_follows (user_uuid, resource_kind, resource_uuid, mode) VALUES ($1, 'doc', $2, 'loud')`,
		uuid.New(), uuid.NewString()); err == nil {
		t.Error("a mode outside all, mentions and none was stored")
	}
	insert := func() error {
		_, err := env.PG.Exec(`INSERT INTO resource_notices (id, user_uuid, kind, resource_kind, resource_uuid, reason, dedup_key)
			VALUES ($1, $2, 'doc.shared', 'doc', 'd', 'subject', 'one-key')`, uuid.New(), uuid.New())
		return err
	}
	if err := insert(); err != nil {
		t.Fatal(err)
	}
	if insert() == nil {
		t.Error("two notices took one dedup key")
	}
}

// docWorld is a private doc with an owner, ten readers and a commenter, each
// with a device, in Postgres and Dgraph.
type docWorld struct {
	doc    string
	people map[string]uuid.UUID
	uids   map[string]string
	mu     sync.Mutex
	pushes [][]string // each push's devices
	titles []string
}

func (w *docWorld) pushed() ([]string, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, p := range w.pushes {
		out = append(out, p...)
	}
	slices.Sort(out)
	return out, slices.Clone(w.titles)
}

func (w *docWorld) clear() {
	w.mu.Lock()
	w.pushes, w.titles = nil, nil
	w.mu.Unlock()
}

func devicesOf(names ...string) []string {
	out := []string{}
	for _, n := range names {
		out = append(out, "device-of-"+n)
	}
	slices.Sort(out)
	return out
}

func TestDocEventsTellTheRightPeopleOnce(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	w := &docWorld{doc: uuid.NewString(), people: map[string]uuid.UUID{}}
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		if data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] != firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT || data[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] != w.doc {
			return
		}
		w.mu.Lock()
		w.pushes = append(w.pushes, slices.Clone(tokens))
		w.titles = append(w.titles, data[firebaseInit.FIREBASE_PUSH_DATA_TITLE])
		w.mu.Unlock()
	})()

	names := []string{"owner", "commenter", "stranger"}
	for i := 0; i < 10; i++ {
		names = append(names, fmt.Sprintf("reader%d", i))
	}
	var nodes []map[string]any
	for _, name := range names {
		id := uuid.New()
		w.people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		if _, err := env.PG.Exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', $2)`, id, "device-of-"+name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	var readers []map[string]any
	for i := 0; i < 10; i++ {
		readers = append(readers, map[string]any{"uid": fmt.Sprintf("_:reader%d", i)})
	}
	// Made now, after docs' mentions began to be kept (migration 344): its first
	// save tells whom its body mentions, as a new doc's does.
	nodes = append(nodes, map[string]any{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": w.doc, "doc_title": "Launch notes",
		"doc_body": "<p>Launch notes</p>", "doc_private": true, "doc_public_comment": true, "doc_deleted_at": "0001-01-01T00:00:00Z",
		"doc_created_at":       time.Now().UTC().Format(time.RFC3339Nano),
		"doc_created_by":       map[string]any{"uid": "_:owner"},
		"doc_editing_users":    []map[string]any{{"uid": "_:owner"}},
		"doc_reading_users":    readers,
		"doc_commenting_users": []map[string]any{{"uid": "_:commenter"}}})
	w.uids = dg.Mutate(t, nodes)

	comment := func(who string, mentions ...string) {
		t.Helper()
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = w.people[who]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: w.uids[who], Uuid: w.people[who].String(), UserName: who}
		d, err := docBusiness.GetBasicDgraphDocByUUID(ctx, w.doc, w.uids[who])
		if err != nil {
			t.Fatal(err)
		}
		var mentioned []*dgraphStruct.DgraphUser
		for _, m := range mentions {
			mentioned = append(mentioned, &dgraphStruct.DgraphUser{Uid: w.uids[m], Uuid: w.people[m].String()})
		}
		w.clear()
		if _, err := docBusiness.CreateDocComment(ctx, d, &u, &docAdapter.CreateOrUpdateDocCommentInput{DocUuid: w.doc, CommentBody: "<p>a comment</p>"}, mentioned); err != nil {
			t.Fatalf("%s commenting: %v", who, err)
		}
		integration.Settle(t)
	}
	notices := func(who, kind string) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM resource_notices WHERE user_uuid = $1 AND kind = $2 AND resource_uuid = $3`,
			w.people[who], kind, w.doc).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	subject := func(who string) accessBusiness.Subject {
		return accessBusiness.Subject{UserUID: w.uids[who], UserUUID: w.people[who].String()}
	}

	// The commenter takes part from here on.
	comment("commenter")

	t.Run("with ten readers and one commenter, only the owner and the commenter hear", func(t *testing.T) {
		comment("reader3")
		got, _ := w.pushed()
		if want := devicesOf("owner", "commenter"); !slices.Equal(got, want) {
			t.Errorf("a reader's comment reached %v, want %v", got, want)
		}
		// The commenter's inbox has it; the owner's feed lists it already.
		if notices("commenter", "doc.comment.created") != 1 || notices("owner", "doc.comment.created") != 0 || notices("reader5", "doc.comment.created") != 0 {
			t.Errorf("inbox notices: commenter %d, owner %d, reader5 %d", notices("commenter", "doc.comment.created"),
				notices("owner", "doc.comment.created"), notices("reader5", "doc.comment.created"))
		}
	})

	t.Run("follow modes", func(t *testing.T) {
		if st, err := followBusiness.Set(ctx, subject("reader5"), "doc", w.doc, followBusiness.ModeAll); err != nil || st.Mode != "all" {
			t.Fatalf("reader5 follows all: %+v %v", st, err)
		}
		if _, err := followBusiness.Set(ctx, subject("owner"), "doc", w.doc, followBusiness.ModeNone); err != nil {
			t.Fatal(err)
		}
		if _, err := followBusiness.Set(ctx, subject("stranger"), "doc", w.doc, followBusiness.ModeAll); err == nil {
			t.Error("someone the private doc isn't shared with followed it")
		}
		comment("reader0")
		got, _ := w.pushed()
		// reader3 commented above, so takes part now.
		if want := devicesOf("commenter", "reader3", "reader5"); !slices.Equal(got, want) {
			t.Errorf("with reader5 following all and the owner none, a comment reached %v, want %v", got, want)
		}
		// A mention reaches the owner, whatever they chose.
		comment("reader0", "owner")
		got, _ = w.pushed()
		if !slices.Contains(got, "device-of-owner") {
			t.Errorf("a mention of the owner, who chose none, reached %v", got)
		}
		if _, err := followBusiness.Clear(ctx, subject("owner"), "doc", w.doc); err != nil {
			t.Fatal(err)
		}
	})

	mentionBody := func(names ...string) string {
		var b strings.Builder
		b.WriteString("<p>Launch notes for ")
		for _, n := range names {
			fmt.Fprintf(&b, `<span data-type="mention" data-id="%s@%s" data-label="%s">@%s</span> `, w.people[n], w.uids[n], n, n)
		}
		b.WriteString("</p>")
		return b.String()
	}

	t.Run("a reader mentioned is told once, and someone who can't read it isn't", func(t *testing.T) {
		w.clear()
		save := func(body string) {
			t.Helper()
			if _, err := docBusiness.SaveFromCollab(ctx, docBusiness.CollabSave{DocUUID: w.doc, HTML: body, Contributors: []string{w.people["owner"].String()}}); err != nil {
				t.Fatal(err)
			}
			integration.Settle(t)
		}
		save(mentionBody("reader1", "stranger"))
		save(mentionBody("reader1", "stranger") + "<p>more</p>")
		if n := notices("reader1", "doc.mentioned"); n != 1 {
			t.Errorf("reader1 has %d mention notices after two saves, want 1", n)
		}
		if n := notices("stranger", "doc.mentioned"); n != 0 {
			t.Errorf("the stranger, who can't read the doc, has %d", n)
		}
		if n := notices("owner", "doc.mention.unshared"); n != 1 {
			t.Errorf("the owner was asked to share %d times, want 1", n)
		}
		got, titles := w.pushed()
		if !slices.Equal(got, devicesOf("reader1")) || len(titles) != 1 || titles[0] != "owner mentioned you in Launch notes" {
			t.Errorf("pushed %v %q", got, titles)
		}
	})

	t.Run("shared with you", func(t *testing.T) {
		w.clear()
		share := func() {
			t.Helper()
			if err := docBusiness.UpdateDocPermissions(ctx, docAdapter.InputUpdateDocPermissions{DocId: w.doc, AddViewers: []string{w.people["stranger"].String()}}, w.uids["owner"]); err != nil {
				t.Fatal(err)
			}
			integration.Settle(t)
		}
		share()
		share() // already a viewer: nothing more
		if n := notices("stranger", "doc.shared"); n != 1 {
			t.Errorf("the stranger has %d shared notices, want 1", n)
		}
		if _, titles := w.pushed(); len(titles) != 1 || titles[0] != "owner shared ‘Launch notes’ with you" {
			t.Errorf("pushed %q", titles)
		}
	})

	t.Run("the inbox lists notices, titled only for who can open the doc", func(t *testing.T) {
		page := activityBusiness.WithNotices(ctx, dgraphModels.UnifiedActivityPagination{}, w.people["reader1"].String(), time.Now().Add(time.Second), 20)
		var found *dgraphModels.NoticeItem
		for _, it := range page.Activities {
			if it.ActivityType == "NOTICE" && it.Notice.Kind == "doc.mentioned" {
				found = it.Notice
			}
		}
		if found == nil || found.Title != "Launch notes" || !found.CanOpen || found.Actor == nil || found.Actor.EmailID != "" {
			t.Fatalf("reader1's inbox: %+v", page.Activities)
		}
		// The sidebar's unread count has it too.
		if n, err := lastSeenActivityBusiness.GetTotalUnreadActivityCount(ctx, w.uids["reader1"], w.people["reader1"]); err != nil || n < 1 {
			t.Errorf("reader1's unread count is %d (%v), want the notice in it", n, err)
		}
		// Taken off the doc: the item stays, untitled.
		if err := docBusiness.UpdateDocPermissions(ctx, docAdapter.InputUpdateDocPermissions{DocId: w.doc, RemoveViewers: []string{w.people["reader1"].String()}}, w.uids["owner"]); err != nil {
			t.Fatal(err)
		}
		integration.Settle(t)
		page = activityBusiness.WithNotices(ctx, dgraphModels.UnifiedActivityPagination{}, w.people["reader1"].String(), time.Now().Add(time.Second), 20)
		for _, it := range page.Activities {
			if it.Notice != nil && (it.Notice.Title != "" || it.Notice.CanOpen) {
				t.Errorf("a doc reader1 can't open is still named: %+v", it.Notice)
			}
		}
	})

	t.Run("every recorded action writes one audit row, and the chain verifies", func(t *testing.T) {
		count := func(action string) int {
			var n int
			if err := env.PG.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE category = 'docs' AND action = $1 AND metadata->>'resource_uuid' = $2`,
				action, w.doc).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		// The share and the removal above.
		if count("doc.shared") != 1 || count("doc.access_changed") != 1 {
			t.Errorf("shared rows %d, access rows %d; want 1 each", count("doc.shared"), count("doc.access_changed"))
		}
		var summary string
		_ = env.PG.QueryRow(`SELECT summary FROM admin_audit_log WHERE action = 'doc.shared' AND metadata->>'resource_uuid' = $1`, w.doc).Scan(&summary)
		if strings.Contains(summary, "Launch notes") {
			t.Errorf("the log names a private page: %q", summary)
		}
		for _, k := range []resourceEventBusiness.Kind{resourceEventBusiness.DocDeleted, resourceEventBusiness.DocRestored,
			resourceEventBusiness.DocLocked, resourceEventBusiness.DocExported, resourceEventBusiness.DocPurged} {
			resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{Kind: k, ResourceKind: "doc", ResourceUUID: w.doc,
				Actor: resourceEventBusiness.Actor{UserUUID: w.people["owner"].String()}})
		}
		integration.Settle(t)
		for _, a := range []string{"doc.deleted", "doc.restored", "doc.locked", "doc.exported", "doc.purged"} {
			if n := count(a); n != 1 {
				t.Errorf("%s wrote %d rows, want 1", a, n)
			}
		}
		res, err := auditBusiness.Verify(ctx)
		if err != nil || res == nil || !res.OK {
			t.Errorf("the chain: %+v %v", res, err)
		}
	})

	t.Run("on the free plan the log is read, not exported", func(t *testing.T) {
		old := helpers.SeatLimit
		helpers.SeatLimit = "25"
		defer func() { helpers.SeatLimit = old }()
		rec := httptest.NewRecorder()
		settingsController.GetAuditLog(rec, httptest.NewRequest(http.MethodGet, "/admin/audit-log?category=docs", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"doc.shared"`) || !strings.Contains(rec.Body.String(), `"docs"`) {
			t.Errorf("reading the docs category on the free plan: %d %.200s", rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		customMiddleware.RequirePlan(helpers.FeatureAuditExport)(http.HandlerFunc(settingsController.ExportAuditLog)).
			ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/audit-log/export?category=docs", nil))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "licence") {
			t.Errorf("exporting on the free plan: %d %.200s", rec.Code, rec.Body.String())
		}
		src, err := os.ReadFile(filepath.Join("..", "..", "router", "router.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, `"/audit-log"`) && strings.Contains(line, "RequirePlan") {
				t.Errorf("reading the log is behind a plan: %s", strings.TrimSpace(line))
			}
		}
	})
}

// A reply in a thread (A8) is told once, to the thread's people and the doc's
// all-followers: not doc-wide as well, and never twice to anyone. The thread's
// first comment is told doc-wide, as any comment is.
func TestAThreadReplyIsToldOnceToItsPeople(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	w := &docWorld{doc: uuid.NewString(), people: map[string]uuid.UUID{}}
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		if data[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] != w.doc {
			return
		}
		w.mu.Lock()
		w.pushes = append(w.pushes, slices.Clone(tokens))
		w.mu.Unlock()
	})()
	var nodes []map[string]any
	for _, name := range []string{"owner", "ann", "bob", "carol", "follower"} {
		id := uuid.New()
		w.people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		if _, err := env.PG.Exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', $2)`, id, "device-of-"+name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	nodes = append(nodes, map[string]any{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": w.doc, "doc_title": "Launch notes",
		"doc_private": true, "doc_public_comment": false, "doc_deleted_at": "0001-01-01T00:00:00Z",
		"doc_created_by":       map[string]any{"uid": "_:owner"},
		"doc_editing_users":    []map[string]any{{"uid": "_:owner"}, {"uid": "_:carol"}},
		"doc_commenting_users": []map[string]any{{"uid": "_:ann"}, {"uid": "_:bob"}},
		"doc_reading_users":    []map[string]any{{"uid": "_:follower"}}})
	w.uids = dg.Mutate(t, nodes)
	as := func(name string) (context.Context, accessBusiness.Subject) {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = w.people[name]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: w.uids[name], Uuid: w.people[name].String(), UserName: name}
		return context.WithValue(ctx, helpers.UserInfoContextKey, u), accessBusiness.Subject{UserUID: w.uids[name], UserUUID: w.people[name].String()}
	}
	_, follower := as("follower")
	if _, err := followBusiness.Set(ctx, follower, "doc", w.doc, followBusiness.ModeAll); err != nil {
		t.Fatal(err)
	}

	annCtx, ann := as("ann")
	made, err := threadBusiness.Threads.Create(annCtx, ann, accessBusiness.Resource{Kind: "doc", UUID: w.doc},
		threadBusiness.Anchor{Kind: threadBusiness.AnchorNone}, threadBusiness.Body{HTML: "<p>Are we sure?</p>"})
	if err != nil {
		t.Fatal(err)
	}
	integration.Settle(t)
	if got, _ := w.pushed(); !slices.Equal(got, devicesOf("owner", "carol", "follower")) {
		t.Errorf("a new thread reached %v, want the doc's people and its follower", got)
	}

	w.clear()
	bobCtx, bob := as("bob")
	reply, err := threadBusiness.Threads.Reply(bobCtx, bob, made.ID, threadBusiness.Body{HTML: "<p>Yes, Thursday.</p>"})
	if err != nil {
		t.Fatal(err)
	}
	integration.Settle(t)
	if got, _ := w.pushed(); !slices.Equal(got, devicesOf("ann", "follower")) {
		t.Errorf("a reply reached %v, want the thread's author and the doc's follower, each once", got)
	}
	var notices int
	if err := env.PG.QueryRow(`SELECT COUNT(*) FROM resource_notices WHERE dedup_key LIKE $1`, "comment|"+reply.ID+"|%").Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if notices != 2 {
		t.Errorf("the reply left %d notices, want one each for ann and the follower", notices)
	}
}
