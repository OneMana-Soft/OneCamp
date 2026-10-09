//go:build integration
// +build integration

package integration_test

// An import that Run has started stays started, whatever a plan does. A plan
// read the job's status and then wrote over whatever it had become: one that
// landed after Run had started the import put it back to planned, and
// planning a failed import again put a running one back to waiting, so Run
// started it a second time (every item made twice). Here Run's start is held,
// uncommitted, until the plan is waiting on the job's row, then let go: the
// plan's write lands just after Run's, every time.
//
// And an import discarded while its file uploaded stays discarded: the
// upload finishing brought it back to waiting, or failed it.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAPlanNeverRestartsAnImport|TestADiscardedUploadStaysDiscarded' -v

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importController "github.com/akashc777/OneCamp/controllers/Import"
	slackImportController "github.com/akashc777/OneCamp/controllers/SlackImport"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// raceProvider is an import source whose plan is one project's chunk.
type raceProvider struct{ importProvider.Provider }

func (raceProvider) Name() string                            { return "racesource" }
func (raceProvider) Capabilities() importProvider.Capability { return importProvider.CapProjects }
func (raceProvider) Validate(context.Context, *importModels.Job, importProvider.JobOptions) error {
	return nil
}
func (raceProvider) Plan(_ context.Context, job *importModels.Job, _ importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	project := "ENG"
	return &importProvider.Plan{ProjectCount: 1}, []*importModels.Chunk{{
		Id: uuid.New(), ImportId: job.Id, ChunkType: "projects", ParentSourceId: &project,
		Status: importModels.ChunkStatusPending, MaxAttempts: 3,
	}}, nil
}
func (raceProvider) CleanupJob(string) {}

// raceEnv is a database with an admin, and a store standing in for MinIO.
type raceEnv struct {
	*integration.Env
	admin   uuid.UUID
	asAdmin context.Context
}

// newRaceEnv serves objects from the store by the end of their keys (any
// other is not found) for the rest of the test.
func newRaceEnv(t *testing.T, objects map[string][]byte) *raceEnv {
	t.Helper()
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	importProvider.Register(raceProvider{})

	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for key, body := range objects {
			if strings.HasSuffix(r.URL.Path, "/"+key) {
				w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef"`)
				http.ServeContent(w, r, path.Base(key), time.Now(), bytes.NewReader(body))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(store.Close)
	endpoint, _ := url.Parse(store.URL)
	client, err := minio.New(endpoint.Host, &minio.Options{
		Creds: credentials.NewStaticV4("test", "test-secret", ""), Secure: false, Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	prevMinio := minioInit.MinioClient
	minioInit.MinioClient = client
	t.Cleanup(func() { minioInit.MinioClient = prevMinio })
	t.Setenv("USER_UPLOAD_BUCKET_NAME", "test-uploads")

	admin := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, 'admin@acme.test')`, admin); err != nil {
		t.Fatal(err)
	}
	return &raceEnv{Env: env, admin: admin, asAdmin: context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: admin, IsAdmin: true},
	})}
}

// call answers h for the admin, with the route's params.
func (e *raceEnv) call(h http.HandlerFunc, params map[string]string) (int, map[string]any) {
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{}`))).WithContext(context.WithValue(e.asAdmin, chi.RouteCtxKey, rc)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// job makes an import of the admin's with its file staged at key.
func (e *raceEnv) job(t *testing.T, provider, label, status, key string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := e.PG.Exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, triggered_by, raw_object_key)
		VALUES ($1, $2, $3, 'export_zip', $4, $5, $6)`, id, provider, label, status, e.admin, key); err != nil {
		t.Fatal(err)
	}
	return id
}

// state is the job's status and how many chunks it has.
func (e *raceEnv) state(t *testing.T, id uuid.UUID) (status string, chunks int) {
	t.Helper()
	if err := e.PG.QueryRow(`SELECT status, (SELECT count(*) FROM import_chunks WHERE import_id = $1) FROM import_jobs WHERE id = $1`, id).Scan(&status, &chunks); err != nil {
		t.Fatal(err)
	}
	return status, chunks
}

// whileHeld answers during while stmt, made on the job's row in a
// transaction, holds the row: the transaction commits once during waits on
// the row, so what during writes lands just after stmt.
func whileHeld(t *testing.T, db *sql.DB, stmt string, id uuid.UUID, during func() (int, map[string]any)) (int, map[string]any) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(stmt, id); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		code int
		out  map[string]any
	}
	done := make(chan answer, 1)
	go func() {
		code, out := during()
		done <- answer{code, out}
	}()
	waitFor(t, "the request to wait on the job", func() bool {
		var waiting int
		_ = db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`).Scan(&waiting)
		return waiting > 0
	})
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	a := <-done
	return a.code, a.out
}

// slackExport is a Slack export with one person and a DM: enough to plan.
func slackExport(t *testing.T) []byte {
	t.Helper()
	var export bytes.Buffer
	zw := zip.NewWriter(&export)
	for name, body := range map[string]string{
		"users.json":    `[{"id":"U1","name":"ana","profile":{}}]`,
		"channels.json": `[]`,
		"dms.json":      `[{"id":"D1","members":["U1"]}]`,
	} {
		f, _ := zw.Create(name)
		_, _ = f.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return export.Bytes()
}

// runStarts is the statement Run's StartRunning makes.
const runStarts = `UPDATE import_jobs SET status = 'running', stage = 'queued', started_at = NOW() WHERE id = $1`

func TestAPlanNeverRestartsAnImport(t *testing.T) {
	ctx := context.Background()
	e := newRaceEnv(t, map[string][]byte{"raw.zip": slackExport(t)})
	const key = "slackImport/x/raw.zip"
	startsAgain := func(id uuid.UUID) bool {
		t.Helper()
		started, err := importModels.StartRunning(ctx, id, []string{importModels.StatusPlanned, importModels.StatusFailed})
		if err != nil {
			t.Fatal(err)
		}
		return started
	}

	for _, c := range []struct {
		name    string
		handler http.HandlerFunc
		job     uuid.UUID
	}{
		{"a Jira-like import's plan landing after Run started it", importController.HandlePlan, e.job(t, "racesource", "Acme", "planned", key)},
		{"a Jira-like import planned again as Run starts it", importController.HandlePlan, e.job(t, "racesource", "Beta", "failed", key)},
		{"a Slack import's plan landing after Run started it", slackImportController.HandlePlan, e.job(t, "slack", "Acme Slack", "planned", key)},
		{"a Slack import planned again as Run starts it", slackImportController.HandlePlan, e.job(t, "slack", "Beta Slack", "failed", key)},
	} {
		code, out := whileHeld(t, e.PG, runStarts, c.job, func() (int, map[string]any) {
			return e.call(c.handler, map[string]string{"jobId": c.job.String()})
		})
		if code != http.StatusConflict || out["code"] != "job_changed" || out["error"] != "This import changed in the meantime." {
			t.Errorf("%s: %d %v, want 409 job_changed", c.name, code, out)
		}
		// Still running, with nothing of the plan's added to the run.
		if status, chunks := e.state(t, c.job); status != "running" || chunks != 0 {
			t.Errorf("%s: left the import %s with %d chunks, want running with none", c.name, status, chunks)
		}
		if startsAgain(c.job) {
			t.Errorf("%s: Run started it a second time", c.name)
		}
	}

	// Planned with nobody racing: stored with its chunks, both kinds.
	for _, c := range []struct {
		name    string
		handler http.HandlerFunc
		job     uuid.UUID
	}{
		{"a Jira-like import", importController.HandlePlan, e.job(t, "racesource", "Gamma", "validating", key)},
		{"a Slack import", slackImportController.HandlePlan, e.job(t, "slack", "Gamma Slack", "validating", key)},
	} {
		if code, out := e.call(c.handler, map[string]string{"jobId": c.job.String()}); code != http.StatusOK {
			t.Fatalf("planning %s: %d %v", c.name, code, out)
		}
		if status, chunks := e.state(t, c.job); status != "planned" || chunks != 1 {
			t.Errorf("%s planned: %s with %d chunks, want planned with 1", c.name, status, chunks)
		}
	}
}

func TestADiscardedUploadStaysDiscarded(t *testing.T) {
	e := newRaceEnv(t, map[string][]byte{"raw.zip": slackExport(t), "empty.zip": {}})
	// The statement Discard makes for an import that never ran.
	const discard = `UPDATE import_jobs SET status = 'cancelled', stage = 'cancelled',
		error_message = 'Discarded before it ran.', completed_at = NOW() WHERE id = $1`
	finalize := map[string]http.HandlerFunc{"racesource": importController.HandleFinalizeUpload, "slack": slackImportController.HandleFinalizeUpload}
	params := func(provider string, id uuid.UUID) map[string]string {
		return map[string]string{"provider": provider, "jobId": id.String()}
	}

	for _, provider := range []string{"racesource", "slack"} {
		// The upload finishes just after the import was discarded: not
		// brought back to waiting.
		discarded := e.job(t, provider, "Acme "+provider, "pending", "imports/a/raw.zip")
		code, out := whileHeld(t, e.PG, discard, discarded, func() (int, map[string]any) {
			return e.call(finalize[provider], params(provider, discarded))
		})
		if code != http.StatusConflict || out["code"] != "job_changed" {
			t.Errorf("%s: an upload finishing after a discard: %d %v, want 409 job_changed", provider, code, out)
		}
		if status, _ := e.state(t, discarded); status != "cancelled" {
			t.Errorf("%s: an import discarded while it uploaded is %s, want cancelled", provider, status)
		}

		// Nor failed, when the file turns out empty.
		empty := e.job(t, provider, "Beta "+provider, "pending", "imports/b/empty.zip")
		if code, out := whileHeld(t, e.PG, discard, empty, func() (int, map[string]any) {
			return e.call(finalize[provider], params(provider, empty))
		}); code != http.StatusBadRequest {
			t.Errorf("%s: an empty upload finishing after a discard: %d %v", provider, code, out)
		}
		if status, _ := e.state(t, empty); status != "cancelled" {
			t.Errorf("%s: an import discarded while an empty file uploaded is %s, want cancelled", provider, status)
		}

		// Nobody racing: it waits to be planned, or fails on an empty file.
		uploaded := e.job(t, provider, "Gamma "+provider, "pending", "imports/c/raw.zip")
		if code, out := e.call(finalize[provider], params(provider, uploaded)); code != http.StatusOK {
			t.Fatalf("%s: finishing an upload: %d %v", provider, code, out)
		}
		if status, _ := e.state(t, uploaded); status != "validating" {
			t.Errorf("%s: an uploaded import is %s, want validating", provider, status)
		}
		emptied := e.job(t, provider, "Delta "+provider, "pending", "imports/d/empty.zip")
		if code, _ := e.call(finalize[provider], params(provider, emptied)); code != http.StatusBadRequest {
			t.Errorf("%s: finishing an empty upload answered %d", provider, code)
		}
		if status, _ := e.state(t, emptied); status != "failed" {
			t.Errorf("%s: an empty upload left the import %s, want failed", provider, status)
		}
	}
}
