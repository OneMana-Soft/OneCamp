//go:build integration

package business

// A public form's files, end to end against Postgres 12, Dgraph and MinIO.
// Run: PKGS=./business/Form/ integ_rel.sh <wt> <log> '^(TestAVisitorsFilesGoOnTheTask|TestAStrangersFileFailsClosed)$'

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	"github.com/akashc777/OneCamp/business/Formula"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/avscan"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

var onePixel = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

// filesForm makes an owner, a project they're in and a form of it whose bug
// reports take screenshots, and answers the form.
func filesForm(t *testing.T, ctx context.Context, dg *integration.DgraphEnv) (*formModel.Form, uuid.UUID, uuid.UUID) {
	t.Helper()
	owner, project := uuid.New(), uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id, email_id, username, display_name) VALUES ($1, $2, $3, $4)`,
		owner, owner.String()+"@example.test", "o-"+owner.String()[:8], "Owner")
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $2)`, project, owner)
	dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "user_uuid": owner.String(), "user_name": "Owner", "dgraph.type": "User"},
		{"uid": "_:team", "team_uuid": uuid.NewString(), "team_name": "Team", "dgraph.type": "Team"},
		{"uid": "_:project", "project_uuid": project.String(), "project_name": "Launch", "dgraph.type": "Project",
			"project_team": map[string]any{"uid": "_:team"}, "project_members": []map[string]any{{"uid": "_:owner"}}},
	})
	f, err := Save(ctx, project, Input{Title: "Launch requests", Priority: "medium", Active: true, TitleField: "what",
		Fields: []Field{
			{Id: "what", Label: "What do you need?", Type: ShortText, Required: true},
			{Id: "shots", Label: "Screenshots", Type: File, Required: true},
		}}, owner)
	if err != nil {
		t.Fatalf("save the form: %v", err)
	}
	return f, owner, project
}

// A visitor's files wait under their claim until the form is sent, then go on
// the task as the project's attachments, owned by the form's owner, their
// objects kept. A file that could run in a browser is refused outright. The
// same files can't be sent twice, and a file whose form was never sent is
// removed a day later, object and all, while sent files stay.
func TestAVisitorsFilesGoOnTheTask(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	dg := integration.SetupDgraph(t)
	client := integration.SetupMinio(t, "form-files-test")
	f, owner, project := filesForm(t, ctx, dg)
	told := make(chan map[string]interface{}, 4)
	oldTell := tellRules
	t.Cleanup(func() { tellRules = oldTell })
	tellRules = func(_ context.Context, data map[string]interface{}) { told <- data }

	pub, err := GetPublic(ctx, f.Token)
	if err != nil || pub.Files == nil || pub.Files.Max != 5 || pub.Files.MB < 1 {
		t.Fatalf("the public form says files %+v, %v", pub, err)
	}
	now := time.Now()
	shot, err := UploadVisitorFile(ctx, f.Token, "", "shots", bytes.NewReader(onePixel), int64(len(onePixel)), "checkout.png", now)
	if err != nil {
		t.Fatalf("upload a screenshot: %v", err)
	}
	second, err := UploadVisitorFile(ctx, f.Token, shot.Claim, "shots", bytes.NewReader(onePixel), int64(len(onePixel)), "cart.png", now)
	if err != nil {
		t.Fatalf("upload a second screenshot: %v", err)
	}
	exe := append([]byte("MZ\x90\x00"), bytes.Repeat([]byte{0}, 64)...)
	if _, err := UploadVisitorFile(ctx, f.Token, shot.Claim, "shots", bytes.NewReader(exe), int64(len(exe)), "setup.exe", now); !errors.Is(err, uploadsafe.ErrUnsupportedType) {
		t.Errorf("a visitor's program: %v, want ErrUnsupportedType", err)
	}
	// An upload nobody sends.
	left, err := UploadVisitorFile(ctx, f.Token, "", "shots", bytes.NewReader(onePixel), int64(len(onePixel)), "left.png", now)
	if err != nil {
		t.Fatalf("upload a file nobody sends: %v", err)
	}

	answers := map[string]any{"what": "Checkout crashes", "shots": []any{shot.File.ID, second.File.ID}}
	if err := Submit(ctx, f.Token, answers, formula.NumberSigns{}, "", shot.Claim, now); err != nil {
		t.Fatalf("send the form: %v", err)
	}
	var ev map[string]interface{}
	select {
	case ev = <-told:
	case <-time.After(5 * time.Second):
		t.Fatal("the rules never heard of the form")
	}
	if got := ev["answers"].(map[string]interface{})["shots"]; got != "checkout.png, cart.png" {
		t.Errorf("a rule reads the files as %v", got)
	}
	q := `query t($id: string) { t(func: eq(task_uuid, $id)) { task_attachments { attachment_uuid attachment_file_name attachment_obj_key attachment_type } } }`
	res, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, q, map[string]string{"$id": ev["task_id"].(string)})
	if err != nil {
		t.Fatal(err)
	}
	var task struct {
		T []struct {
			Files []struct {
				ID   string `json:"attachment_uuid"`
				Name string `json:"attachment_file_name"`
				Key  string `json:"attachment_obj_key"`
				Type string `json:"attachment_type"`
			} `json:"task_attachments"`
		} `json:"t"`
	}
	if err := json.Unmarshal(res.Json, &task); err != nil || len(task.T) != 1 || len(task.T[0].Files) != 2 {
		t.Fatalf("the task's files: %s %v", res.Json, err)
	}
	for _, file := range task.T[0].Files {
		if file.Type != "image" || !strings.HasPrefix(file.Key, "forms/"+f.Id.String()+"/") {
			t.Errorf("a file on the task: %+v", file)
		}
		var src, val, by string
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT src_key, src_value, created_by::text FROM attachments WHERE id = $1`, file.ID).Scan(&src, &val, &by); err != nil {
			t.Fatalf("the attachment row of %s: %v", file.Name, err)
		}
		if src != "project" || val != project.String() || by != owner.String() {
			t.Errorf("%s is the attachment of %s %s by %s", file.Name, src, val, by)
		}
		if _, err := client.StatObject(ctx, "form-files-test", "userFileUpload/"+file.Key, minio.StatObjectOptions{}); err != nil {
			t.Errorf("%s isn't in the bucket: %v", file.Name, err)
		}
	}
	var fe *FormError
	if err := Submit(ctx, f.Token, answers, formula.NumberSigns{}, "", shot.Claim, now); !errors.As(err, &fe) {
		t.Errorf("the same files sent twice: %v, want a FormError", err)
	}

	// A day later the sweep removes the file nobody sent; the sent ones stay.
	if err := purgeUnsent(ctx, now.Add(uploadsKeptFor+time.Minute)); err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	var waiting int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM form_uploads WHERE form_id = $1`, f.Id).Scan(&waiting); err != nil || waiting != 0 {
		t.Errorf("%d rows left after the sweep, %v", waiting, err)
	}
	gone := "forms/" + f.Id.String() + "/" + left.File.ID + "_left.png"
	if _, err := client.StatObject(ctx, "form-files-test", "userFileUpload/"+gone, minio.StatObjectOptions{}); err == nil {
		t.Error("a file nobody sent is still in the bucket")
	}
	for _, file := range task.T[0].Files {
		if _, err := client.StatObject(ctx, "form-files-test", "userFileUpload/"+file.Key, minio.StatObjectOptions{}); err != nil {
			t.Errorf("the sweep took the task's %s: %v", file.Name, err)
		}
	}
}

// brokenScanner is a scanner that can't say.
type brokenScanner struct{}

func (brokenScanner) Scan(_ context.Context, r io.Reader) (avscan.Result, error) {
	_, _ = io.Copy(io.Discard, r)
	return avscan.Result{}, errors.New("clamd: connection refused")
}

// Where the install fails open, a member's file is kept when the scanner
// can't say, but a visitor's never is: nothing of it stays in the bucket.
func TestAStrangersFileFailsClosed(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	dg := integration.SetupDgraph(t)
	client := integration.SetupMinio(t, "form-scan-test")
	f, owner, _ := filesForm(t, ctx, dg)
	t.Setenv("AVSCAN_FAIL_OPEN", "true")
	avscan.SetDefault(brokenScanner{})
	t.Cleanup(func() { avscan.SetDefault(nil) })

	if _, err := UploadVisitorFile(ctx, f.Token, "", "shots", bytes.NewReader(onePixel), int64(len(onePixel)), "shot.png", time.Now()); err == nil {
		t.Fatal("a visitor's file was kept while the scanner couldn't say")
	}
	for obj := range client.ListObjects(ctx, "form-scan-test", minio.ListObjectsOptions{Prefix: "userFileUpload/forms/", Recursive: true}) {
		t.Errorf("a refused visitor's file stayed: %s", obj.Key)
	}
	member, err := attachmentBusiness.StoreUpload(helpers.WithSystemRead(ctx), bytes.NewReader(onePixel), int64(len(onePixel)), "shot.png", owner,
		attachmentBusiness.StoreOptions{Safe: uploadsafe.Options{AllowInlineImages: true}})
	if err != nil || member.ObjKey == "" {
		t.Fatalf("a member's file where the install fails open: %+v %v", member, err)
	}
}
