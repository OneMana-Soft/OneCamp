//go:build integration
// +build integration

package integration_test

// The docs programme's workspace export (C4) against real stores: the export
// consents' migration up, down and up again on Postgres 12, and a workspace
// export of real docs into MinIO, with what its admin can open and what
// owners let in. The live doc's read is the one fake. (The import's pages
// stage has its own test, in business/Import pages_integration_test.go.)
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestDocExportConsents|TestAWorkspaceExport' -v

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	contentIOController "github.com/akashc777/OneCamp/controllers/ContentIO"
	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/akashc777/OneCamp/tests/integration"
)

// TestDocExportConsentsMigrationUpDownUp: the migration goes up, down and up
// again on Postgres 12, with a number a programme migration may have, and
// ids made by the application.
func TestDocExportConsentsMigrationUpDownUp(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(here), "..", "..", "migrations")
	read := func(direction string) (string, string) {
		matches, _ := filepath.Glob(filepath.Join(dir, "*_doc_export_consents."+direction+".sql"))
		if len(matches) != 1 {
			t.Fatalf("want one doc_export_consents %s migration, found %v", direction, matches)
		}
		b, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Base(matches[0]), string(b)
	}
	upName, up := read("up")
	_, down := read("down")
	if n, err := strconv.Atoi(strings.SplitN(upName, "_", 2)[0]); err != nil || n == 198 || n < 205 {
		t.Fatalf("%s: a docs programme migration is numbered above 204, and never 198", upName)
	}
	if strings.Contains(up, "gen_random_uuid") || strings.Contains(up, "uuid_generate") {
		t.Fatalf("%s makes ids in the database; Postgres 12 has neither function by default", upName)
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
		if tableExists(t, env.PG, "doc_export_consents") != step.want {
			t.Fatalf("step %d: doc_export_consents exists = %v", i, !step.want)
		}
	}
	// A consent names its owner, who must exist.
	if _, err := env.PG.Exec(`INSERT INTO doc_export_consents (doc_uuid, owner_uuid) VALUES ($1, $2)`, uuid.New(), uuid.New()); err == nil {
		t.Fatal("a consent from nobody was kept")
	}
}

// docsWorld is the stores the docs tests need: Postgres, Redis, the graph, a
// search server that accepts everything, and people.
type docsWorld struct {
	env    *integration.Env
	dg     *integration.DgraphEnv
	people map[string]uuid.UUID
	uids   map[string]string
}

func newDocsWorld(t *testing.T, names []string, admins ...string) *docsWorld {
	t.Helper()
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
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	w := &docsWorld{env: env, dg: dg, people: map[string]uuid.UUID{}}
	var nodes []map[string]any
	for _, n := range names {
		w.people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			w.people[n], w.people[n].String()+"@example.test", n, n); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": w.people[n].String(), "user_name": n,
			"user_full_name": n, "user_deleted_at": "0001-01-01T00:00:00Z"})
	}
	for _, a := range admins {
		if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id) VALUES ($1)`, w.people[a].String()+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	w.uids = dg.Mutate(t, nodes)
	return w
}

func (w *docsWorld) subject(name string) accessBusiness.Subject {
	return accessBusiness.Subject{UserUID: w.uids[name], UserUUID: w.people[name].String()}
}

// call runs a route's handler as a person, with body as its JSON and jobID as
// its {job_id}.
func (w *docsWorld) call(handler http.HandlerFunc, name, body, jobID string) *httptest.ResponseRecorder {
	info := userModels.UserInfo{}
	info.UserPostgresInfo.Id = w.people[name]
	info.UserPostgresInfo.EmailID = w.people[name].String() + "@example.test"
	info.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: w.uids[name], Uuid: w.people[name].String()}
	route := chi.NewRouteContext()
	route.URLParams.Add("job_id", jobID)
	ctx := context.WithValue(context.WithValue(context.Background(), helpers.UserInfoContextKey, info), chi.RouteCtxKey, route)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// audited waits for the audit log to hold action by the person: entries are
// written as the request ends.
func (w *docsWorld) audited(t *testing.T, action, name string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		var n int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE action = $1 AND actor_id = $2`, action, w.people[name]).Scan(&n); err == nil && n > 0 {
			return
		}
	}
	t.Fatalf("no %s by %s in the audit log", action, name)
}

// doc makes a doc for a person: in the workspace, or private to them.
func (w *docsWorld) doc(t *testing.T, owner, title string, workspace bool) string {
	t.Helper()
	space := ""
	if workspace {
		space = "workspace"
	}
	d, err := docBusiness.CreateDocInTree(context.Background(), &dgraphStruct.DgraphUser{Uid: w.uids[owner]}, docBusiness.NewDoc{Title: title, Space: space}, nil)
	if err != nil || d == nil {
		t.Fatalf("make %q: %v", title, err)
	}
	return d.Uuid
}

// ai-search-io.md P2-4: an admin's workspace export holds every doc the admin
// can open and the private docs whose owners let them in, as Markdown, and
// says how many private docs it left out.
func TestAWorkspaceExportHoldsWhatItsAdminCanOpenAndWhatOwnersLetIn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newDocsWorld(t, []string{"ada", "maya"}, "ada")
	startMinio(t)
	w.doc(t, "maya", "Handbook", true)
	w.doc(t, "maya", "Diary", false)
	plans := w.doc(t, "maya", "Plans", false)
	w.doc(t, "ada", "Admin notes", false)
	savedRead := collab.Read
	t.Cleanup(func() { collab.Read = savedRead })
	collab.Read = func(_ context.Context, id string) (collab.Current, error) {
		return collab.Current{HTML: "<p>Body of " + id + "</p>"}, nil
	}

	// Maya lets Plans in; only she may, and an admin who can't open it is told
	// nothing of it.
	if rec := w.call(contentIOController.SetExportConsent, "ada", `{"doc_uuid":"`+plans+`","allow":true}`, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("an admin let someone else's private doc into exports: %d", rec.Code)
	}
	if rec := w.call(contentIOController.SetExportConsent, "maya", `{"doc_uuid":"`+plans+`","allow":true}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("Maya's consent: %d %s", rec.Code, rec.Body)
	}
	if _, err := contentIOBusiness.ExportWorkspace(ctx, w.subject("maya")); err != contentIOBusiness.ErrNotAdmin {
		t.Fatalf("a member's workspace export: %v", err)
	}
	rec := w.call(contentIOController.StartWorkspaceExport, "ada", "", "")
	var started struct{ Export contentIOBusiness.Job }
	if err := json.Unmarshal(rec.Body.Bytes(), &started); rec.Code != http.StatusAccepted || err != nil {
		t.Fatalf("starting the export: %d %s", rec.Code, rec.Body)
	}
	job := started.Export
	// The queue's runner takes the export within a few of its own intervals of
	// starting, even after an earlier start whose context has ended (another
	// test's, earlier in this process), as such a test leaves the sweeps: that
	// once left the queue unread, and the export sat queued until this test
	// gave up.
	ended, end := context.WithCancel(ctx)
	end()
	sweepBusiness.Jobs.StartAll(ended)
	sweepBusiness.Jobs.StartAll(ctx)
	// The first pass is one interval after the start; the rest is room for a
	// loaded machine.
	claimWithin := 5 * contentIOBusiness.QueuePollEvery
	var err error
	for claimBy := time.Now().Add(claimWithin); ; time.Sleep(100 * time.Millisecond) {
		if job, _, err = contentIOBusiness.WorkspaceExport(ctx, w.subject("ada"), job.ID); err != nil {
			t.Fatal(err)
		}
		if job.Status != contentIOBusiness.JobQueued {
			break
		}
		if time.Now().After(claimBy) {
			t.Fatalf("the export was still queued %v after the workers started (the queue is read every %v): nothing took it", claimWithin, contentIOBusiness.QueuePollEvery)
		}
	}
	var url string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		job, url, err = contentIOBusiness.WorkspaceExport(ctx, w.subject("ada"), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == contentIOBusiness.JobDone || job.Status == contentIOBusiness.JobFailed {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if job.Status != contentIOBusiness.JobDone || url == "" || job.Exported != 3 {
		t.Fatalf("the export ended %+v, download %q", job, url)
	}
	rec = w.call(contentIOController.GetWorkspaceExport, "ada", "", job.ID)
	var download struct {
		URL string `json:"download_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &download); rec.Code != http.StatusOK || err != nil || !strings.HasPrefix(download.URL, "http") {
		t.Fatalf("the download: %d %s", rec.Code, rec.Body)
	}
	// Starting it, letting a private doc in and downloading it are each in
	// the audit log, by who did it.
	for action, who := range map[string]string{"docs.export_consent": "maya", "docs.workspace_export": "ada", "docs.workspace_export_download": "ada"} {
		w.audited(t, action, who)
	}
	if len(job.Skipped) != 1 || job.Skipped[0].What != "1 private doc" {
		t.Fatalf("left out %+v", job.Skipped)
	}

	obj, err := minioInit.MinioClient.GetObject(ctx, helpers.UserUploadBucket(), "content-io/"+job.ID+"/workspace-docs.zip", minio.GetObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(obj)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	var names []string
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(b)
		names = append(names, f.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "Admin notes.md,Handbook.md,Plans.md" {
		t.Fatalf("the zip holds %v", names)
	}
	if files["Plans.md"] != "# Plans\n\nBody of "+plans+"\n" || strings.Contains(strings.Join(names, ","), "Diary") {
		t.Fatalf("Plans.md is %q", files["Plans.md"])
	}
}
