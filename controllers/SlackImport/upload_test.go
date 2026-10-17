package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// An export object storage didn't take, because storage isn't answering,
// answers as every stored file does (helpers.WriteStorageFailure): 503, in
// words the upload dialog shows ("msg"), saying to try again in a moment. It
// answered 500 with an "error" key the app doesn't read: a store that was down
// for a moment read as the server's fault, and said nothing about trying again.
func TestAnUploadObjectStorageDidntTakeAsksToTryAgain(t *testing.T) {
	// Object storage at a port nothing listens on: the write fails at once,
	// tried once rather than with minio's ten backed-off retries.
	store, err := minio.New("127.0.0.1:1", &minio.Options{Creds: credentials.NewStaticV4("key", "secret", ""), Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	oldStore, oldRetry := minioInit.MinioClient, minio.MaxRetry
	minioInit.MinioClient, minio.MaxRetry = store, 1
	t.Cleanup(func() { minioInit.MinioClient, minio.MaxRetry = oldStore, oldRetry })

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("slack_workspace_name", "Acme"); err != nil {
		t.Fatal(err)
	}
	file, err := form.CreateFormFile("file", "acme-export.zip")
	if err != nil {
		t.Fatal(err)
	}
	// A ZIP's signature, so the upload gets as far as the store.
	if _, err := file.Write(append([]byte("PK\x03\x04"), make([]byte, 60)...)); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	admin := userModel.UserInfo{UserPostgresInfo: userModel.User{IsAdmin: true, Id: uuid.New()}}
	req := httptest.NewRequest(http.MethodPost, "/", &body).
		WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, admin))
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	HandleUpload(rec, req)

	var out struct {
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusServiceUnavailable || out.Msg != "File storage isn't answering right now. Try again in a moment." {
		t.Errorf("object storage unreachable, the upload answered %d %s; want 503 saying to try again in a moment", rec.Code, rec.Body.String())
	}
}
