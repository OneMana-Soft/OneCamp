//go:build integration
// +build integration

package integration_test

// Chat's uploads keep their answers through the shared store
// (business/Attachment StoreUpload, 934574411), through POST /uploadFile, on
// MinIO and Postgres 12:
//   - a file for a channel, and one for a DM, each by its uploader's path
//     (channel_uuids/chat_uuids, and src_key/src_value): 200 with its id, its
//     attachments row at the place with the key <uploader>/<id>_<name>, and
//     its bytes stored under userFileUpload/ with a forced download;
//   - a file past the workspace's limit: 413 naming the limit, nothing stored;
//   - a file the AV scan finds infected: 422 in words, no row, and its bytes
//     removed.
//
// Run: integ_rel.sh <wt> <log> '^TestChatUploadsKeepTheirAnswersThroughTheSharedStore$'

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"

	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/avscan"
	"github.com/akashc777/OneCamp/initializers/minioInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// infectedScanner finds a virus in everything it reads.
type infectedScanner struct{}

func (infectedScanner) Scan(_ context.Context, r io.Reader) (avscan.Result, error) {
	n, _ := io.Copy(io.Discard, r)
	return avscan.Result{Verdict: avscan.VerdictInfected, Signature: "Eicar-Test-Signature", Bytes: n}, nil
}

func TestChatUploadsKeepTheirAnswersThroughTheSharedStore(t *testing.T) {
	p := setUpPlacesForFiles(t)
	ctx := context.Background()
	upload := func(jsonData, name string, body []byte) (int, map[string]any) {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, _ := mw.CreateFormFile("file", name)
		_, _ = part.Write(body)
		_ = mw.WriteField("jsonData", jsonData)
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/uploadFile", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		var u userModels.UserInfo
		u.UserPostgresInfo = p.as("mia").UserPostgresInfo
		req = req.WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, u))
		rec := httptest.NewRecorder()
		userController.UploadFile(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	stored := func() []string {
		t.Helper()
		var keys []string
		for o := range minioInit.MinioClient.ListObjects(ctx, helpers.UserUploadBucket(), minio.ListObjectsOptions{Prefix: "userFileUpload/" + p.people["mia"].String() + "/", Recursive: true}) {
			if o.Err != nil {
				t.Fatal(o.Err)
			}
			keys = append(keys, o.Key)
		}
		return keys
	}
	mia := p.people["mia"].String()

	for _, c := range []struct{ what, jsonData, srcKey, srcValue string }{
		{"a channel's file", `{"channel_uuids":["` + p.channel + `"]}`, "channel", p.channel},
		{"a DM's file", `{"chat_uuids":["` + p.dmMiaAnn + `"]}`, "chat", p.dmMiaAnn},
		{"a DM's file named as one place", place("chat", p.dmMiaAnn), "chat", p.dmMiaAnn},
	} {
		code, out := upload(c.jsonData, "plan.txt", []byte("the plan"))
		id, _ := out["object_uuid"].(string)
		if code != http.StatusOK || id == "" {
			t.Fatalf("%s: %d %v", c.what, code, out)
		}
		var key, srcKey, srcValue, by string
		if err := p.env.PG.QueryRow(`SELECT obj_key, src_key, src_value, created_by FROM attachments WHERE id = $1`, id).Scan(&key, &srcKey, &srcValue, &by); err != nil {
			t.Fatalf("%s: its row: %v", c.what, err)
		}
		if key != mia+"/"+id+"_plan.txt" || srcKey != c.srcKey || srcValue != c.srcValue || by != mia {
			t.Errorf("%s: its row names %q at %s/%s by %s", c.what, key, srcKey, srcValue, by)
		}
		info, err := minioInit.MinioClient.StatObject(ctx, helpers.UserUploadBucket(), "userFileUpload/"+key, minio.StatObjectOptions{})
		if err != nil || info.Size != int64(len("the plan")) || !strings.HasPrefix(info.ContentType, "text/plain") ||
			!strings.HasPrefix(info.Metadata.Get("Content-Disposition"), "attachment") {
			t.Errorf("%s: its bytes: %+v %v", c.what, info, err)
		}
	}
	before := stored()

	t.Setenv("UPLOAD_LIMIT_IN_MB", "1")
	code, out := upload(`{"channel_uuids":["`+p.channel+`"]}`, "big.txt", bytes.Repeat([]byte("x"), 1<<20+1))
	if code != http.StatusRequestEntityTooLarge || out["upload_limit_mb"] != float64(1) || out["error"] != "file_too_large" ||
		!strings.Contains(out["msg"].(string), "1 MB") {
		t.Errorf("past the limit: %d %v", code, out)
	}

	avscan.SetDefault(infectedScanner{})
	t.Cleanup(func() { avscan.SetDefault(nil) })
	var rows int
	if err := p.env.PG.QueryRow(`SELECT count(*) FROM attachments WHERE created_by = $1`, p.people["mia"]).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	code, out = upload(`{"chat_uuids":["`+p.dmMiaAnn+`"]}`, "invoice.txt", []byte("X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR"))
	if code != http.StatusUnprocessableEntity || !strings.Contains(out["msg"].(string), "virus") {
		t.Errorf("an infected file: %d %v", code, out)
	}
	var after int
	if err := p.env.PG.QueryRow(`SELECT count(*) FROM attachments WHERE created_by = $1`, p.people["mia"]).Scan(&after); err != nil || after != rows {
		t.Errorf("an infected file left a row: %d -> %d (%v)", rows, after, err)
	}
	if left := stored(); len(left) != len(before) {
		t.Errorf("an infected file's bytes, or a too large one's, were kept: %v (before %v)", left, before)
	}
}
