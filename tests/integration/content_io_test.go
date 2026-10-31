//go:build integration
// +build integration

package integration_test

// The docs programme's import pipeline (A7) against real stores: its
// migration (content_io_jobs, found by name) up, down and up again on
// Postgres 12; two workers never claiming the same job; and a zip of three
// files (Markdown, HTML, Word) imported into Dgraph
// docs, with every image uploaded to MinIO as the doc's attachment and none
// left in a body. The Live Doc Writer is the one fake: it records the HTML
// each doc is seeded with.
//
// Run: go test -tags=integration ./tests/integration/ -run TestContentIO -v

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	contentIOModels "github.com/akashc777/OneCamp/models/postgres/ContentIO"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/akashc777/OneCamp/tests/integration"
)

// contentIOMigration is the content_io_jobs migration's file in one
// direction, found by name: the integrator numbers migrations in merge order
// (A7 reserved 209), so the test never names the number.
func contentIOMigration(t *testing.T, dir string) (string, string) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_content_io_jobs."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one content_io_jobs %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(matches[0]), string(b)
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var ok bool
	if err := db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// The content_io_jobs migration goes up, down and up again on Postgres 12,
// its number is one a programme migration may have, and its ids come from
// the application (no gen_random_uuid).
func TestContentIOMigrationUpDownUp(t *testing.T) {
	upName, up := contentIOMigration(t, "up")
	_, down := contentIOMigration(t, "down")
	// 198 is Connect's and never used. 199 lies below 200–204, which were
	// taken first, and the boot check compares only the highest applied, so
	// a migration numbered under 205 could go missing unnoticed.
	if n, err := strconv.Atoi(strings.SplitN(upName, "_", 2)[0]); err != nil || n == 198 || n < 205 {
		t.Fatalf("%s: a docs programme migration is numbered above 204, and never 198", upName)
	}
	makesNoIDs(t, upName, up)

	env := integration.SetupEnv(t)
	if !tableExists(t, env.PG, "content_io_jobs") {
		t.Fatal("content_io_jobs is missing after every migration ran")
	}
	for i, step := range []struct {
		sql  string
		want bool
	}{{down, false}, {up, true}, {down, false}, {up, true}} {
		if _, err := env.PG.Exec(step.sql); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if tableExists(t, env.PG, "content_io_jobs") != step.want {
			t.Fatalf("step %d: content_io_jobs exists = %v", i, !step.want)
		}
	}
	// No default id: the row must bring its own.
	user := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'u', 'u', NOW(), NOW())`, user, user.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO content_io_jobs (kind, user_uuid) VALUES ('import', $1)`, user); err == nil {
		t.Fatal("a job with no id was accepted")
	}
	if _, err := env.PG.Exec(`INSERT INTO content_io_jobs (id, kind, user_uuid, status) VALUES ($1, 'import', $2, 'lost')`, uuid.New(), user); err == nil {
		t.Fatal("a status outside the CHECK was accepted")
	}
}

// A job waits, ready, for its person's choice (tables S1's preview): only a
// running job becomes ready, keeping what its file holds; a choice queues it
// again with the choices in its params once, never twice; one left
// unconfirmed past its time fails, so its file can go. The ready status's
// migration goes down and up again: a job still ready fails on the way down.
func TestContentIOJobsWaitReadyForAChoice(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	user := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'u', 'u', NOW(), NOW())`, user, user.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	key := "content-io/ready/in"
	job := &contentIOModels.Job{ID: uuid.New(), Kind: "import", Format: "sheet", UserUUID: user, Status: contentIOModels.StatusQueued, InputKey: &key}
	if err := contentIOModels.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	preview := json.RawMessage(`{"preview":{"sheets":[]}}`)
	if err := contentIOModels.Ready(ctx, job.ID, preview); err != nil {
		t.Fatal(err)
	}
	if got, _ := contentIOModels.Get(ctx, job.ID); got.Status != contentIOModels.StatusQueued {
		t.Fatalf("a queued job became %s; only a running one becomes ready", got.Status)
	}
	if claimed, err := contentIOModels.ClaimNext(ctx); err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("claimed %+v, %v", claimed, err)
	}
	if err := contentIOModels.Ready(ctx, job.ID, preview); err != nil {
		t.Fatal(err)
	}
	ready, err := contentIOModels.Get(ctx, job.ID)
	if err != nil || ready.Status != contentIOModels.StatusReady || ready.Progress != 100 || !strings.Contains(string(ready.Result), `"sheets"`) {
		t.Fatalf("the ready job is %+v (%v)", ready, err)
	}
	params := json.RawMessage(`{"filename":"Deals.xlsx","choices":{"sheets":[]},"confirmed":true}`)
	for i, want := range []bool{true, false} {
		chose, err := contentIOModels.Choose(ctx, job.ID, params)
		if err != nil || chose != want {
			t.Fatalf("choice %d: %v (%v), want %v", i+1, chose, err, want)
		}
	}
	queued, _ := contentIOModels.Get(ctx, job.ID)
	if queued.Status != contentIOModels.StatusQueued || !strings.Contains(string(queued.Params), `"confirmed": true`) || string(queued.Result) != "{}" {
		t.Fatalf("after the choice the job is %s with params %s and result %s", queued.Status, queued.Params, queued.Result)
	}

	// Left unconfirmed past its time, a ready job fails.
	if _, err := contentIOModels.ClaimNext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := contentIOModels.Ready(ctx, job.ID, preview); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	dead, err := contentIOModels.FailAbandoned(ctx, now.Add(-time.Hour), now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil || len(dead) != 1 || dead[0].ID != job.ID || dead[0].Status != contentIOModels.StatusFailed || dead[0].Error == nil || !strings.Contains(*dead[0].Error, "Nobody confirmed") {
		t.Fatalf("the unconfirmed job's cleanup: %+v (%v)", dead, err)
	}

	// The migration down fails a job still ready, and refuses the status;
	// up again takes it.
	again := &contentIOModels.Job{ID: uuid.New(), Kind: "import", Format: "sheet", UserUUID: user, Status: contentIOModels.StatusRunning, InputKey: &key}
	if err := contentIOModels.Create(ctx, again); err != nil {
		t.Fatal(err)
	}
	if err := contentIOModels.Ready(ctx, again.ID, preview); err != nil {
		t.Fatal(err)
	}
	up, down := readyMigration(t, "up"), readyMigration(t, "down")
	if _, err := env.PG.Exec(down); err != nil {
		t.Fatal(err)
	}
	if got, _ := contentIOModels.Get(ctx, again.ID); got.Status != contentIOModels.StatusFailed {
		t.Errorf("down left a ready job %s, want failed", got.Status)
	}
	if _, err := env.PG.Exec(`UPDATE content_io_jobs SET status = 'ready' WHERE id = $1`, again.ID); err == nil {
		t.Error("down still takes the ready status")
	}
	if _, err := env.PG.Exec(up); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`UPDATE content_io_jobs SET status = 'ready' WHERE id = $1`, again.ID); err != nil {
		t.Errorf("up again doesn't take the ready status: %v", err)
	}
}

// readyMigration is the ready status's migration, up or down.
func readyMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_content_io_ready."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one content_io_ready %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Two workers asking at once take different jobs, oldest first.
func TestContentIOJobsAreClaimedOnce(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	user := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'u', 'u', NOW(), NOW())`, user, user.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("content-io/%d/in", i)
		if err := contentIOModels.Create(ctx, &contentIOModels.Job{ID: uuid.New(), Kind: "import", Format: "md", UserUUID: user, Status: contentIOModels.StatusQueued, InputKey: &key}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	got := map[uuid.UUID]int{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := contentIOModels.ClaimNext(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			if j != nil {
				mu.Lock()
				got[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != 2 {
		t.Fatalf("claimed %v; want both jobs, once each", got)
	}
	for id, n := range got {
		if n != 1 {
			t.Fatalf("job %s was claimed %d times", id, n)
		}
	}
}

// liveRecorder is the Live Doc Writer, recording what each doc is seeded with.
type liveRecorder struct {
	collab.Writer
	mu     sync.Mutex
	seeded map[string]string
}

func (l *liveRecorder) Seed(_ context.Context, id, html string, _ collab.Options) (collab.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seeded[id] = html
	return collab.Result{PersistedAt: time.Now()}, nil
}

// startMinio gives the docs' tests their uploads bucket (integration.SetupMinio).
func startMinio(t *testing.T) {
	t.Helper()
	integration.SetupMinio(t, "content-io-test")
}

var onePixelPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

func wordFile(t *testing.T) []byte {
	t.Helper()
	const ns = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"`
	return zipBytes(t, map[string][]byte{
		"word/document.xml": []byte(`<?xml version="1.0"?><w:document ` + ns + `><w:body>` +
			`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Report</w:t></w:r></w:p>` +
			`<w:p><w:r><w:t>Numbers went up.</w:t></w:r></w:p>` +
			`<w:p><w:r><w:drawing><a:blip r:embed="rId1"/></w:drawing></w:r></w:p></w:body></w:document>`),
		"word/styles.xml":              []byte(`<?xml version="1.0"?><w:styles ` + ns + `><w:style w:styleId="Heading1"><w:name w:val="heading 1"/></w:style></w:styles>`),
		"word/_rels/document.xml.rels": []byte(`<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="image" Target="media/chart.png"/></Relationships>`),
		"word/media/chart.png":         onePixelPNG,
	})
}

func zipBytes(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, data := range files {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A zip of three files makes three docs, each with its image in storage.
func TestContentIOZipOfThreeFilesMakesThreeDocsWithImagesInStorage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
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

	person := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'sam', 'Sam', NOW(), NOW())`, person, person.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	uids := dg.Mutate(t, []map[string]any{{"uid": "_:sam", "dgraph.type": "User", "user_uuid": person.String(), "user_name": "sam",
		"user_full_name": "Sam", "user_deleted_at": "0001-01-01T00:00:00Z"}})
	who := accessBusiness.Subject{UserUID: uids["sam"], UserUUID: person.String()}

	data := zipBytes(t, map[string][]byte{
		"Notes.md":        []byte("# Launch notes\n\n- [ ] book the room\n\n![logo](images/logo.png)\n"),
		"Setup.html":      []byte(`<title>Setup</title><p>Install <b>it</b>.</p><img src="images/logo.png" alt="logo">`),
		"Report.docx":     wordFile(t),
		"images/logo.png": onePixelPNG,
	})
	job, err := contentIOBusiness.Pipeline.Import(ctx, who, contentIOBusiness.ImportRequest{
		Filename: "wiki.zip", Size: int64(len(data)), Body: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	sweepBusiness.Jobs.StartAll(ctx)
	job = waitForContentIOJob(ctx, t, who, job.ID)
	if job.Status != contentIOBusiness.JobDone || len(job.Created) != 3 {
		raw, _ := json.Marshal(job)
		t.Fatalf("the import ended %s", raw)
	}
	titles := map[string]bool{}
	for _, c := range job.Created {
		titles[c.Title] = true
		html := live.seeded[c.UUID]
		if strings.Contains(html, "base64") || strings.Contains(html, "blob:") {
			t.Errorf("%s: a body holds an image's bytes: %s", c.Title, html)
		}
		if !strings.Contains(html, "/doc/getDocAttachment/"+c.UUID+"/") {
			t.Errorf("%s: its image is not the doc's attachment: %s", c.Title, html)
		}
		// The image is in storage, recorded as this doc's.
		var objKey string
		if err := env.PG.QueryRow(`SELECT obj_key FROM attachments WHERE src_key = 'doc' AND src_value = $1`, c.UUID).Scan(&objKey); err != nil {
			t.Errorf("%s: no attachment row: %v", c.Title, err)
			continue
		}
		if _, err := minioInit.MinioClient.StatObject(ctx, helpers.UserUploadBucket(), "userFileUpload/"+objKey, minio.StatObjectOptions{}); err != nil {
			t.Errorf("%s: its image is not in MinIO: %v", c.Title, err)
		}
		// And the doc is in the graph, theirs, with its title.
		doc, err := docBusiness.GetDocByDocUUID(ctx, c.UUID, who.UserUID)
		if err != nil || doc == nil || doc.Title != c.Title {
			t.Errorf("%s: the doc is not in the graph as made: %+v %v", c.Title, doc, err)
		}
	}
	for _, want := range []string{"Launch notes", "Setup", "Report"} {
		if !titles[want] {
			t.Errorf("no doc titled %q among %v", want, titles)
		}
	}
	// The upload is gone once the job is done.
	row, err := contentIOModels.Get(ctx, uuid.MustParse(job.ID))
	if err != nil || row.InputKey != nil {
		t.Errorf("the job still names its upload: %+v %v", row, err)
	}

	// A file whose content the document service refuses leaves no doc: the
	// doc made for it goes to the trash and out again (A3's TrashSubtree and
	// PurgeDoc). The sweep started above runs this job too.
	refuser := &liveRefuser{}
	collab.Live = refuser
	refused := []byte("# Refused\n\nNothing of this is kept.\n")
	job, err = contentIOBusiness.Pipeline.Import(ctx, who, contentIOBusiness.ImportRequest{
		Filename: "refused.md", Size: int64(len(refused)), Body: bytes.NewReader(refused),
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	job = waitForContentIOJob(ctx, t, who, job.ID)
	if job.Status != contentIOBusiness.JobFailed || len(job.Created) != 0 || len(refuser.asked) != 1 {
		raw, _ := json.Marshal(job)
		t.Fatalf("the refused import ended %s, after %d seeds", raw, len(refuser.asked))
	}
	left, err := docBusiness.DocRows(ctx, refuser.asked, who.UserUID)
	if err != nil || len(left) != 0 {
		t.Fatalf("the doc it could not fill is still in the graph: %+v %v", left, err)
	}
}

// waitForContentIOJob is the job once it has ended, or as it stands after 90 seconds.
func waitForContentIOJob(ctx context.Context, t *testing.T, who accessBusiness.Subject, id string) contentIOBusiness.Job {
	t.Helper()
	var job contentIOBusiness.Job
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		job, err = contentIOBusiness.Pipeline.Job(ctx, who, id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == contentIOBusiness.JobDone || job.Status == contentIOBusiness.JobFailed {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	return job
}

// liveRefuser is a Live Doc Writer that refuses every seed, and records the
// docs it was asked to fill.
type liveRefuser struct {
	collab.Writer
	mu    sync.Mutex
	asked []string
}

func (l *liveRefuser) Seed(_ context.Context, id, _ string, _ collab.Options) (collab.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, id)
	return collab.Result{}, errors.New("live write: refused")
}
