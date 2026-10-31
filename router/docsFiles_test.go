package router

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"

	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// The doc tree's file uploads (docs F1) get past /doc's 8 MiB body cap, which
// a file under the workspace's upload limit (10 MB by default, up to 5 GB)
// can be larger than: their two multipart routes are on the Files router,
// whose handlers cap the body at the upload limit themselves
// (httpx.ReadUpload). Built as router.go builds it: /doc mounted inside a
// group capped at 8 MiB. A 9 MiB upload reaches its handler whole, a 9 MiB
// body to any other /doc route is still refused, and the versions' GET, which
// takes no upload, is still /doc's.
func TestTheFileUploadsGetPastTheDocsBodyCap(t *testing.T) {
	root, doc := chi.NewRouter(), chi.NewRouter()
	root.Group(func(r chi.Router) {
		r.Use(customMiddleware.BodyLimit(8 << 20))
		r.Mount("/doc", doc)
	})
	doc.Post("/other", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	read := func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		_, _ = w.Write([]byte(strconv.FormatInt(n, 10)))
	}
	listed := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("versions")) }
	mountFileRoutes(docsRouters{Doc: doc, Files: root.With()}, read, read, listed)

	big := bytes.Repeat([]byte("x"), 9<<20)
	for _, path := range []string{"/doc/files", "/doc/files/5f0c4a2e-8a63-4f3e-9d2b-1c7a9e0b3d41/versions"} {
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(big)))
		if rec.Code != http.StatusOK || rec.Body.String() != strconv.Itoa(9<<20) {
			t.Errorf("POST %s with 9 MiB: %d %q; want the whole body read", path, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/doc/other", bytes.NewReader(big)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("another /doc route took 9 MiB: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/doc/files/5f0c4a2e-8a63-4f3e-9d2b-1c7a9e0b3d41/versions", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "versions" {
		t.Errorf("GET the versions: %d %q; want /doc's route", rec.Code, rec.Body.String())
	}
}
