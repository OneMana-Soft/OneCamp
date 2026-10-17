package router

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// A4's routes: the collaboration service's save answers only the internal secret,
// and takes no body past its cap.
func TestTheCollabSaveIsInternalAndCapped(t *testing.T) {
	const secret = "a-test-secret-long-enough-to-count-0123456789"
	t.Setenv("INTERNAL_SECRET", secret)
	root := chi.NewRouter()
	doc, admin, docColab := chi.NewRouter(), chi.NewRouter(), chi.NewRouter()
	root.Mount("/doc", doc)
	root.Mount("/docColab", docColab)
	mountDocVersions(newDocsRouters(root, doc, admin, docColab, chi.NewRouter()))

	rec := httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/docColab/updateDoc", bytes.NewReader([]byte(`{}`))))
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("no secret: %d", rec.Code)
	}

	big := bytes.Repeat([]byte("x"), docColabSaveLimit+1)
	req := httptest.NewRequest(http.MethodPost, "/docColab/updateDoc", bytes.NewReader(append(append([]byte(`{"htmlContent":"`), big...), '"', '}')))
	req.Header.Set("X-Internal-Secret", secret)
	rec = httptest.NewRecorder()
	root.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Errorf("a body past the cap: %d", rec.Code)
	}
}
