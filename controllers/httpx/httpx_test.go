package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestMustUserAndRequireAdmin(t *testing.T) {
	msg := func(rec *httptest.ResponseRecorder) string {
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body["msg"]
	}
	rec := httptest.NewRecorder()
	if _, ok := MustUser(rec, httptest.NewRequest(http.MethodGet, "/", nil)); ok || rec.Code != http.StatusUnauthorized || msg(rec) != helpers.UnauthorizedMessage {
		t.Fatalf("nobody signed in: ok %v, %d %q", ok, rec.Code, msg(rec))
	}
	member := userModel.UserInfo{}
	member.UserPostgresInfo.EmailID = "m@example.test"
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, member))
	rec = httptest.NewRecorder()
	if u, ok := MustUser(rec, r); !ok || u.UserPostgresInfo.EmailID != "m@example.test" {
		t.Fatalf("a member: %v %+v", ok, u)
	}
	rec = httptest.NewRecorder()
	if _, ok := RequireAdmin(rec, r); ok || rec.Code != http.StatusForbidden {
		t.Fatalf("a member isn't an admin: %v %d", ok, rec.Code)
	}
	admin := member
	admin.UserPostgresInfo.IsAdmin = true
	r = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, &admin))
	rec = httptest.NewRecorder()
	if _, ok := RequireAdmin(rec, r); !ok {
		t.Fatalf("an admin (held as a pointer): %d", rec.Code)
	}
}

// An upload is read under the workspace's limit (upload_limit_mb): a body
// past it is cut off before anything is parsed and answered 413, naming the
// limit, as the web's "File exceeds the N MB limit" toast reads it; a form
// with no file is a 400; a file within it comes back with its name.
func TestAnUploadIsReadUnderTheWorkspacesLimit(t *testing.T) {
	post := func(size int, field string) *http.Request {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		if field != "" {
			part, _ := mw.CreateFormFile(field, "msa.pdf")
			_, _ = part.Write(bytes.Repeat([]byte("x"), size))
		}
		_ = mw.WriteField("note", "Signed")
		_ = mw.Close()
		r := httptest.NewRequest(http.MethodPost, "/doc/files", &body)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		return r
	}
	w := httptest.NewRecorder()
	_, _, ok := ReadUpload(w, post(1<<20+1, "file"), 1)
	var refused map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &refused)
	if ok || w.Code != http.StatusRequestEntityTooLarge || refused["upload_limit_mb"] != float64(1) || refused["error"] != "file_too_large" ||
		!strings.Contains(refused["msg"].(string), "1 MB") {
		t.Errorf("past the limit: %v %d %s", ok, w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	if _, _, ok := ReadUpload(w, post(0, ""), 1); ok || w.Code != http.StatusBadRequest {
		t.Errorf("no file: %v %d %s", ok, w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	r := post(1000, "file")
	f, h, ok := ReadUpload(w, r, 1)
	if !ok || f == nil || h.Filename != "msa.pdf" || h.Size != 1000 || r.FormValue("note") != "Signed" {
		t.Errorf("a file within it: %v %+v", ok, h)
	}
	if f != nil {
		_ = f.Close()
	}
}

// A JSON body is read up to its limit; one that isn't JSON, or runs past the
// limit, is answered 400 in words and nothing is kept.
func TestAJSONBodyIsReadUpToItsLimit(t *testing.T) {
	type form struct {
		Title string `json:"title"`
	}
	read := func(body string, limit int64) (form, bool, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		var in form
		ok := ReadJSON(w, httptest.NewRequest(http.MethodPost, "/tables/t/forms", strings.NewReader(body)), limit, &in)
		return in, ok, w
	}
	if in, ok, w := read(`{"title":"Lead sign-up"}`, 64); !ok || in.Title != "Lead sign-up" || w.Body.Len() != 0 {
		t.Errorf("within the limit: %v %+v %s", ok, in, w.Body)
	}
	for name, body := range map[string]string{
		"not JSON":       `title=Lead`,
		"past the limit": `{"title":"` + strings.Repeat("x", 64) + `"}`,
		"empty":          ``,
	} {
		in, ok, w := read(body, 64)
		var answer map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &answer)
		if ok || w.Code != http.StatusBadRequest || answer["msg"] != "Couldn't read that request." || in.Title != "" {
			t.Errorf("%s: %v %d %s %+v", name, ok, w.Code, w.Body, in)
		}
	}
}
