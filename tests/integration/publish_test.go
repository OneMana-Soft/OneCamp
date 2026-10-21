//go:build integration
// +build integration

package integration_test

// A doc's "Anyone with the link" grant, published as a web page (docs C6,
// D-25), against real Postgres 12, Dgraph and MinIO. One external-access
// model: migration guest_grant_publish goes up, down and up on the grant
// table; every link already sent keeps working (a view link, minted the old
// way, opens the server's page; a comment link keeps opening the app); the
// owner publishes a view link at an address; one switch stops links and pages
// alike, its one sub-option the pages alone; a password opens the page by
// either way in and nothing goes round it; revoke and unpublish end the page
// at once; an expired address says so; a member copies a page that lets
// copies; each change is the grant's own audit.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestPublish' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	formBusiness "github.com/akashc777/OneCamp/business/Form"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	formController "github.com/akashc777/OneCamp/controllers/Form"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	publishController "github.com/akashc777/OneCamp/controllers/Publish"
	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/akashc777/OneCamp/tests/integration"
)

// publishMigration is the guest_grant_publish migration in one direction,
// found by name, so a renumbering at merge keeps the test.
func publishMigration(t *testing.T, dir string) (string, string) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_guest_grant_publish."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one guest_grant_publish %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(matches[0]), string(b)
}

// columnExists is calendar_attendance_test.go's, shared by the package.

// The migration goes up, down and up on Postgres 12; an address has its
// shape, and is unique among links nobody revoked.
func TestPublishMigrationUpDownUp(t *testing.T) {
	upName, up := publishMigration(t, "up")
	_, down := publishMigration(t, "down")
	if n, err := strconv.Atoi(strings.SplitN(upName, "_", 2)[0]); err != nil || n == 198 || n < 205 {
		t.Fatalf("%s: a docs programme migration is numbered above 204, and never 198", upName)
	}
	if strings.Contains(up, "gen_random_uuid") {
		t.Fatalf("%s uses gen_random_uuid; Postgres 12 has none by default", upName)
	}
	env := integration.SetupEnv(t)
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		if columnExists(t, env, "guest_grants", "slug") != step.want {
			t.Fatalf("step %d: guest_grants.slug exists = %v", i, !step.want)
		}
	}
	user := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'u', 'u', NOW(), NOW())`, user, user.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	grant := func(slug string) (uuid.UUID, error) {
		id := uuid.New()
		_, err := env.PG.Exec(`INSERT INTO guest_grants (id, token_hash, resource_type, resource_id, capability, created_by, slug) VALUES ($1, $2, 'doc', $3, 'view', $4, $5)`,
			id, []byte(id.String()), uuid.NewString(), user, slug)
		return id, err
	}
	first, err := grant("launch-notes")
	if err != nil {
		t.Fatalf("a good address: %v", err)
	}
	if _, err := grant("launch-notes"); err == nil {
		t.Error("two live links share an address")
	}
	if _, err := env.PG.Exec(`UPDATE guest_grants SET revoked_at = NOW() WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	if _, err := grant("launch-notes"); err != nil {
		t.Errorf("a revoked link's address can't be used again: %v", err)
	}
	for _, bad := range []string{"Launch", "-a", "a-", "a b", strings.Repeat("a", 65), "../x"} {
		if _, err := grant(bad); err == nil {
			t.Errorf("the address %q was kept", bad)
		}
	}
	var version int
	var indexable, copies bool
	if err := env.PG.QueryRow(`SELECT unlock_version, indexable, allow_copies FROM guest_grants WHERE slug = 'launch-notes' AND revoked_at IS NULL`).Scan(&version, &indexable, &copies); err != nil ||
		version != 1 || indexable || copies {
		t.Errorf("a link's defaults: version %d, indexable %v, copies %v, %v", version, indexable, copies, err)
	}
}

func TestPublishedLinksEndToEnd(t *testing.T) {
	ctx := context.Background()
	t.Setenv("JWT_SECRET", "integration-publish-secret")
	t.Setenv("BACKEND_DOMAIN", "https://api.example.test")
	t.Setenv("FRONTEND_DOMAIN", "https://app.example.test")
	t.Setenv("TRUST_PROXY_HEADERS", "")
	t.Setenv("GUEST_ACCESS_ENABLED", "")
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	settingsBusiness.ForgetSettingsForTest()
	t.Cleanup(settingsBusiness.ForgetSettingsForTest)
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
	startMinio(t)
	live := &liveRecorder{seeded: map[string]string{}}
	savedLive := collab.Live
	collab.Live = live
	t.Cleanup(func() { collab.Live = savedLive })

	// The owner's doc, someone else's doc, a picture of each.
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, name := range []string{"owner", "member"} {
		id := uuid.New()
		people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			id, name+"-"+id.String()[:8]+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name,
			"user_full_name": name, "user_deleted_at": "0001-01-01T00:00:00Z"})
	}
	doc, otherDoc := uuid.NewString(), uuid.NewString()
	own, theirs := uuid.New(), uuid.New()
	ownURL := "https://api.example.test/doc/getDocAttachment/" + doc + "/" + own.String()
	theirURL := "https://api.example.test/doc/getDocAttachment/" + otherDoc + "/" + theirs.String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	nodes = append(nodes,
		map[string]any{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "Launch notes", "doc_private": true,
			"doc_body": `<p>We ship on Friday, says <span data-type="mention" data-id="uid-secret">@Priya</span>.</p>` +
				`<img src="` + ownURL + `" alt="Roadmap"><img src="` + theirURL + `" alt="Salaries">` +
				`<div data-type="table-embed" data-table="table-secret"></div>`,
			"doc_deleted_at": "0001-01-01T00:00:00Z", "doc_created_at": now, "doc_updated_at": now,
			"doc_created_by": map[string]any{"uid": "_:owner"}, "doc_owner": map[string]any{"uid": "_:owner"}},
		map[string]any{"uid": "_:other", "dgraph.type": "Doc", "doc_uuid": otherDoc, "doc_title": "Salaries", "doc_private": true,
			"doc_body": "<p>Private.</p>", "doc_deleted_at": "0001-01-01T00:00:00Z", "doc_created_at": now, "doc_updated_at": now,
			"doc_created_by": map[string]any{"uid": "_:member"}, "doc_owner": map[string]any{"uid": "_:member"}})
	dg := integration.SetupDgraph(t)
	uids := dg.Mutate(t, nodes)
	for _, f := range []struct {
		id        uuid.UUID
		doc, name string
	}{{own, doc, "roadmap.png"}, {theirs, otherDoc, "salaries.png"}} {
		key := people["owner"].String() + "/" + f.id.String() + "_" + f.name
		if _, err := env.PG.Exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, 'doc', $3, $4)`,
			f.id, key, f.doc, people["owner"]); err != nil {
			t.Fatal(err)
		}
		if _, err := minioInit.MinioClient.PutObject(ctx, helpers.UserUploadBucket(), "userFileUpload/"+key, bytes.NewReader(onePixelPNG),
			int64(len(onePixelPNG)), minio.PutObjectOptions{ContentType: "image/png"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}

	userOf := func(name string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name]
		u.UserPostgresInfo.EmailID = name + "@example.test"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: people[name].String(), UserName: name}
		return u
	}
	h := chi.NewRouter()
	h.Use(customMiddleware.SecurityHeaders)
	h.Post("/guest/collab/{token}", guestController.GuestCollabToken)
	h.Get("/guest/doc-comments/{token}", guestController.GuestDocComments)
	h.Get("/guest/doc-attachment/{token}/{obj_uuid}", guestController.GuestDocAttachment)
	h.Get("/public/p/{slug}", publishController.PublishedPage)
	h.Post("/public/p/{slug}/unlock", publishController.PublishedPageUnlock)
	h.Get("/public/p/{slug}/files/{obj_uuid}", publishController.PublishedFile)
	h.Get("/public/d/{token}", publishController.LinkPage)
	h.Post("/public/d/{token}/unlock", publishController.LinkPageUnlock)
	h.Get("/public/d/{token}/files/{obj_uuid}", publishController.LinkFile)
	m := chi.NewRouter()
	m.Put("/guest/links/{id}/publish", guestController.PublishGuestLink)
	m.Delete("/guest/links/{id}/publish", guestController.UnpublishGuestLink)
	m.Post("/guest/links/{id}/revoke", guestController.RevokeResourceGuestLink)
	m.Post("/guest/links/copy", publishController.Duplicate)
	call := func(handler http.Handler, method, target, body string, who *userModels.UserInfo) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.RemoteAddr = "203.0.113.9:40000"
		if who != nil {
			r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, *who))
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	get := func(target string) *httptest.ResponseRecorder { return call(h, http.MethodGet, target, "", nil) }
	data := func(w *httptest.ResponseRecorder) map[string]any {
		var out struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out.Data
	}

	// Links minted the old way, as clients have them in their email today.
	view, err := guestBusiness.CreateResourceGrant(ctx, people["owner"], "doc", doc, "view", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	comment, err := guestBusiness.CreateResourceGrant(ctx, people["owner"], "doc", doc, "comment", 0, true)
	if err != nil {
		t.Fatal(err)
	}

	// /guest/d/<token>, a view link: the web asks the collaboration exchange,
	// which sends it to the server's page; no collaboration token.
	ex := call(h, http.MethodPost, "/guest/collab/"+view.Token, `{}`, nil)
	if d := data(ex); ex.Code != http.StatusOK || d["page_url"] != "https://api.example.test/public/d/"+view.Token || d["collab_token"] != nil {
		t.Fatalf("a view link's exchange: %d %s", ex.Code, ex.Body)
	}
	page := get("/public/d/" + view.Token)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, "We ship on Friday") || !strings.Contains(body, "/public/d/"+view.Token+"/files/"+own.String()) ||
		!strings.Contains(body, `class="fresh"`) || strings.Contains(body, `rel="canonical"`) {
		t.Fatalf("a view link's page: %d\n%s", page.Code, body)
	}
	for _, secret := range []string{"uid-secret", "table-secret", otherDoc, theirs.String(), "getDocAttachment", "<script"} {
		if strings.Contains(body, secret) {
			t.Errorf("the page shows %q", secret)
		}
	}
	if w := get("/public/d/" + view.Token + "/files/" + own.String()); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), onePixelPNG) {
		t.Errorf("a view link's picture: %d", w.Code)
	}
	if w := get("/public/d/" + view.Token + "/files/" + theirs.String()); w.Code != http.StatusNotFound {
		t.Errorf("another doc's file under the link: %d", w.Code)
	}
	// The app's guest routes refuse a view link: one renderer.
	if w := get("/guest/doc-attachment/" + view.Token + "/" + own.String()); w.Code != http.StatusNotFound {
		t.Errorf("a view link's picture through the app's route: %d", w.Code)
	}
	if w := get("/guest/doc-comments/" + view.Token); w.Code != http.StatusNotFound {
		t.Errorf("a view link's comments through the app's route: %d", w.Code)
	}

	// /guest/d/<token>, a comment link: still the app, with its collaboration
	// token, its pictures and its comments; never a page.
	ex = call(h, http.MethodPost, "/guest/collab/"+comment.Token, `{}`, nil)
	if d := data(ex); ex.Code != http.StatusOK || d["collab_token"] == nil || d["page_url"] != nil {
		t.Errorf("a comment link's exchange: %d %s", ex.Code, ex.Body)
	}
	if w := get("/public/d/" + comment.Token); w.Code != http.StatusNotFound {
		t.Errorf("a comment link as a page: %d", w.Code)
	}
	if w := get("/guest/doc-comments/" + comment.Token); w.Code != http.StatusOK {
		t.Errorf("a comment link's comments: %d", w.Code)
	}
	if w := get("/guest/doc-attachment/" + comment.Token + "/" + own.String()); w.Code != http.StatusTemporaryRedirect {
		t.Errorf("a comment link's picture: %d", w.Code)
	}

	// The owner publishes the view link; someone who isn't the owner can't.
	owner, member := userOf("owner"), userOf("member")
	if w := call(m, http.MethodPut, "/guest/links/"+view.GrantID.String()+"/publish", `{}`, &member); w.Code != http.StatusNotFound && w.Code != http.StatusForbidden {
		t.Errorf("a member publishes the owner's doc: %d %s", w.Code, w.Body)
	}
	if w := call(m, http.MethodPut, "/guest/links/"+comment.GrantID.String()+"/publish", `{}`, &owner); w.Code != http.StatusBadRequest {
		t.Errorf("a comment link published: %d %s", w.Code, w.Body)
	}
	pub := call(m, http.MethodPut, "/guest/links/"+view.GrantID.String()+"/publish", `{"slug":"Launch Notes","allow_copies":true}`, &owner)
	if d := data(pub); pub.Code != http.StatusOK || d["slug"] != "launch-notes" || d["url"] != "https://app.example.test/p/launch-notes" {
		t.Fatalf("publish: %d %s", pub.Code, pub.Body)
	}
	byAddress := get("/public/p/launch-notes")
	if byAddress.Code != http.StatusOK || !strings.Contains(byAddress.Body.String(), `<link rel="canonical" href="https://api.example.test/public/p/launch-notes">`) ||
		byAddress.Header().Get("X-Robots-Tag") != "noindex, nofollow" || !strings.Contains(byAddress.Body.String(), `href="https://app.example.test/p/launch-notes?copy=1"`) {
		t.Fatalf("the published page: %d %v\n%s", byAddress.Code, byAddress.Header(), byAddress.Body)
	}

	// A member copies it: private, theirs, its picture the copy's own.
	cp := call(m, http.MethodPost, "/guest/links/copy", `{"slug":"launch-notes"}`, &member)
	var copied struct {
		DocUUID string `json:"doc_uuid"`
	}
	_ = json.Unmarshal(cp.Body.Bytes(), &copied)
	if cp.Code != http.StatusOK || copied.DocUUID == "" {
		t.Fatalf("copy: %d %s", cp.Code, cp.Body)
	}
	if seeded := live.seeded[copied.DocUUID]; !strings.Contains(seeded, "/doc/getDocAttachment/"+copied.DocUUID+"/") || strings.Contains(seeded, "table-secret") {
		t.Errorf("the copy's body: %s", seeded)
	}
	var copyFile string
	if err := env.PG.QueryRow(`SELECT id::text FROM attachments WHERE src_key = 'doc' AND src_value = $1`, copied.DocUUID).Scan(&copyFile); err != nil {
		t.Fatalf("the copy's picture: %v", err)
	}
	if _, err := fileAccessBusiness.FileOf(ctx, fileAccessBusiness.Place{Kind: "doc", ID: copied.DocUUID}, copyFile); err != nil {
		t.Errorf("the copy's picture isn't its own: %v", err)
	}
	if d, err := accessBusiness.Rules.Resolve(ctx, accessBusiness.Subject{UserUID: uids["member"], UserUUID: people["member"].String()},
		accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: copied.DocUUID}); err != nil || d.Level != accessBusiness.LevelFull {
		t.Errorf("the copy is the member's: %+v %v", d, err)
	}

	// One switch stops links and pages alike; its sub-option the pages alone.
	if err := settingsBusiness.SetGuestAccessEnabled(false); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/public/p/launch-notes", "/public/d/" + view.Token} {
		if w := get(target); w.Code != http.StatusNotFound {
			t.Errorf("%s with guest access off: %d", target, w.Code)
		}
	}
	if w := call(h, http.MethodPost, "/guest/collab/"+comment.Token, `{}`, nil); w.Code != http.StatusNotFound {
		t.Errorf("a comment link with guest access off: %d", w.Code)
	}
	_ = settingsBusiness.SetGuestAccessEnabled(true)
	if err := settingsBusiness.SetGuestPublishEnabled(false); err != nil {
		t.Fatal(err)
	}
	if w := get("/public/p/launch-notes"); w.Code != http.StatusNotFound {
		t.Errorf("the address with publishing off: %d", w.Code)
	}
	if w := get("/public/d/" + view.Token); w.Code != http.StatusOK {
		t.Errorf("the link's page with publishing off: %d", w.Code)
	}
	_ = settingsBusiness.SetGuestPublishEnabled(true)

	// A password opens the page by either way in, and nothing goes round it.
	pw := call(m, http.MethodPut, "/guest/links/"+view.GrantID.String()+"/publish", `{"password":"open sesame","allow_copies":true}`, &owner)
	if pw.Code != http.StatusOK || data(pw)["has_password"] != true {
		t.Fatalf("a password: %d %s", pw.Code, pw.Body)
	}
	for _, target := range []string{"/public/p/launch-notes", "/public/d/" + view.Token} {
		if w := get(target); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without its password: %d", target, w.Code)
		}
	}
	if w := call(h, http.MethodPost, "/guest/collab/"+view.Token, `{}`, nil); data(w)["collab_token"] != nil {
		t.Errorf("a password link's exchange gave a collaboration token: %s", w.Body)
	}
	if w := call(h, http.MethodPost, "/public/p/launch-notes/unlock", `{"password":"wrong"}`, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a wrong password: %d", w.Code)
	}
	unlocked := call(h, http.MethodPost, "/public/d/"+view.Token+"/unlock", `{"password":"open sesame"}`, nil)
	k, _ := data(unlocked)["token"].(string)
	if unlocked.Code != http.StatusOK || k == "" {
		t.Fatalf("the right password: %d %s", unlocked.Code, unlocked.Body)
	}
	for _, target := range []string{"/public/p/launch-notes?k=" + k, "/public/d/" + view.Token + "?k=" + k} {
		if w := get(target); w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "private, no-store" {
			t.Errorf("%s unlocked: %d %v", target, w.Code, w.Header())
		}
	}

	// Unpublishing ends the address at once; the link's token keeps its page.
	if w := call(m, http.MethodDelete, "/guest/links/"+view.GrantID.String()+"/publish", ``, &owner); w.Code != http.StatusOK {
		t.Fatalf("unpublish: %d %s", w.Code, w.Body)
	}
	if w := get("/public/p/launch-notes"); w.Code != http.StatusNotFound {
		t.Errorf("the address after an unpublish: %d", w.Code)
	}
	if w := get("/public/d/" + view.Token); w.Code != http.StatusOK {
		t.Errorf("the link's page after an unpublish (its password went with it): %d", w.Code)
	}

	// An expired address says so; an expired token stays the uniform answer.
	if w := call(m, http.MethodPut, "/guest/links/"+view.GrantID.String()+"/publish", `{"slug":"launch-notes"}`, &owner); w.Code != http.StatusOK {
		t.Fatalf("publish again: %d %s", w.Code, w.Body)
	}
	if _, err := env.PG.Exec(`UPDATE guest_grants SET expires_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, view.GrantID); err != nil {
		t.Fatal(err)
	}
	if w := get("/public/p/launch-notes"); w.Code != http.StatusGone {
		t.Errorf("an expired address: %d", w.Code)
	}
	if w := get("/public/d/" + view.Token); w.Code != http.StatusNotFound {
		t.Errorf("an expired token: %d", w.Code)
	}
	if _, err := env.PG.Exec(`UPDATE guest_grants SET expires_at = NULL WHERE id = $1`, view.GrantID); err != nil {
		t.Fatal(err)
	}

	// Revoking the link ends the token and the address at once.
	if w := call(m, http.MethodPost, "/guest/links/"+view.GrantID.String()+"/revoke", ``, &owner); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	for _, target := range []string{"/public/p/launch-notes", "/public/d/" + view.Token, "/public/d/" + view.Token + "/files/" + own.String()} {
		if w := get(target); w.Code != http.StatusNotFound {
			t.Errorf("%s after a revoke: %d", target, w.Code)
		}
	}

	// Each change is the grant's own audit.
	deadline := time.Now().Add(10 * time.Second)
	for _, action := range []string{"guest.link.publish", "guest.link.unpublish", "guest.link.revoke"} {
		for {
			var n int
			_ = env.PG.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE category = 'security' AND action = $1 AND metadata->>'grant_id' = $2`, action, view.GrantID.String()).Scan(&n)
			if n > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("no %s row in the audit log", action)
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	integration.Settle(t)
}

// Every link already sent lands where it did: one test per URL form, each
// with a token minted the old way. A doc's view link (/guest/d/<token>, now
// the server's page) and its comment link (still the app) are
// TestPublishedLinksEndToEnd's; a table's, a channel's, a project's and a
// meeting's (/guest/t|c|p|m/<token>) are TestGuestLinks'; a booking page's
// (/book/<slug>, /booking/<token>) TestBookingPages'. Here: a board's link
// (/guest/b/<token>) still opens the app's live board, and the collaboration
// service still lets it in, as it lets a doc's comment link in; an intake
// form (/f/<token>) is its project's own link, untouched by guest grants and
// by publishing.
func TestEveryLinkAlreadySentLandsOnItsView(t *testing.T) {
	ctx := context.Background()
	t.Setenv("JWT_SECRET", "integration-links-secret")
	t.Setenv("BACKEND_DOMAIN", "https://api.example.test")
	t.Setenv("FRONTEND_DOMAIN", "https://app.example.test")
	t.Setenv("GUEST_ACCESS_ENABLED", "")
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	settingsBusiness.ForgetSettingsForTest()
	t.Cleanup(settingsBusiness.ForgetSettingsForTest)
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	owner := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'olu', 'Olu', NOW(), NOW())`,
		owner, "olu-"+owner.String()[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	zero := "0001-01-01T00:00:00Z"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	board, doc, project := uuid.NewString(), uuid.NewString(), uuid.New()
	dg.Mutate(t, []map[string]any{
		{"uid": "_:olu", "dgraph.type": "User", "user_uuid": owner.String(), "user_name": "olu", "user_full_name": "Olu",
			"user_deleted_at": zero, "user_projects": []map[string]any{{"uid": "_:project"}}},
		{"uid": "_:board", "dgraph.type": "Board", "board_uuid": board, "board_title": "Launch plan", "board_private": true,
			"board_deleted_at": zero, "board_created_by": map[string]any{"uid": "_:olu"}},
		{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "Launch brief", "doc_private": true, "doc_body": "<p>Brief.</p>",
			"doc_deleted_at": zero, "doc_created_at": now, "doc_updated_at": now, "doc_created_by": map[string]any{"uid": "_:olu"}},
		{"uid": "_:project", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Website",
			"project_deleted_at": zero, "project_members": []map[string]any{{"uid": "_:olu"}},
			"project_team": map[string]any{"dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Studio"}},
	})
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	h := chi.NewRouter()
	h.Post("/guest/collab/{token}", guestController.GuestCollabToken)
	h.Get("/public/form/{token}", formController.GetPublicForm)
	call := func(method, target string) (int, map[string]any, string) {
		r := httptest.NewRequest(method, target, strings.NewReader(`{}`))
		r.RemoteAddr = "203.0.113.9:40000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Data, w.Body.String()
	}

	// /guest/b/<token>: the app's live board, as before; the collaboration
	// service lets that board in, and only that board.
	boardLink, err := guestBusiness.CreateResourceGrant(ctx, owner, "board", board, "view", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	code, d, body := call(http.MethodPost, "/guest/collab/"+boardLink.Token)
	live, _ := d["collab_token"].(string)
	if code != http.StatusOK || live == "" || d["document_name"] != "board:"+board || d["resource_type"] != "board" || d["page_url"] != nil {
		t.Fatalf("a board link's exchange: %d %s", code, body)
	}
	if g, err := guestBusiness.AuthorizeGuestCollab(ctx, live, "board", board); err != nil || g == nil || g.Id != boardLink.GrantID {
		t.Errorf("the collaboration service refuses the board link: %+v %v", g, err)
	}
	if _, err := guestBusiness.AuthorizeGuestCollab(ctx, live, "board", uuid.NewString()); err == nil {
		t.Error("the board link's live token opens another board")
	}

	// /guest/d/<token>, a comment link: the app's live doc, let in as before.
	commentLink, err := guestBusiness.CreateResourceGrant(ctx, owner, "doc", doc, "comment", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	code, d, body = call(http.MethodPost, "/guest/collab/"+commentLink.Token)
	live, _ = d["collab_token"].(string)
	if code != http.StatusOK || live == "" || d["document_name"] != doc || d["page_url"] != nil {
		t.Fatalf("a comment link's exchange: %d %s", code, body)
	}
	if g, err := guestBusiness.AuthorizeGuestCollab(ctx, live, "doc", doc); err != nil || g == nil || g.Id != commentLink.GrantID {
		t.Errorf("the collaboration service refuses the comment link: %+v %v", g, err)
	}

	// /f/<token>: an intake form, as its project's admins made it, opens
	// whether pages may be published or not.
	form, err := formBusiness.Save(project, formBusiness.Input{
		Title:  "Requests",
		Fields: []formBusiness.Field{{Id: "what", Label: "What do you need?", Type: formBusiness.ShortText, Required: true}},
		Active: true,
	}, owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, publishing := range []bool{true, false} {
		if err := settingsBusiness.SetGuestPublishEnabled(publishing); err != nil {
			t.Fatal(err)
		}
		if code, d, body := call(http.MethodGet, "/public/form/"+form.Token); code != http.StatusOK || d["title"] != "Requests" {
			t.Errorf("the intake form with publishing %v: %d %s", publishing, code, body)
		}
	}
	integration.Settle(t)
}
