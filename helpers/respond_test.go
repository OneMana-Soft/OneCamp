package helpers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func TestPageAndUUIDParams(t *testing.T) {
	q := url.Values{"pageIndex": {"3"}, "pageSize": {"500"}}
	if p := PageParam(q, 20, 100); p.Index != 3 || p.Size != 100 || p.Offset() != 300 {
		t.Fatalf("page: %+v", p)
	}
	if p := PageParam(url.Values{"pageIndex": {"-2"}}, 20, 100); p.Index != 0 || p.Size != 20 {
		t.Fatalf("a negative index is the first page: %+v", p)
	}
	id := uuid.New()
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id.String())
	rctx.URLParams.Add("bad", "nope")
	r := httptest.NewRequest(http.MethodGet, "/?q="+id.String(), nil)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	if got, ok := UUIDParam(rec, r, "id"); !ok || got != id {
		t.Fatalf("id: %v %v", got, ok)
	}
	if _, ok := UUIDParam(rec, r, "bad"); ok || rec.Code != http.StatusBadRequest {
		t.Fatalf("not an id: %v %d", ok, rec.Code)
	}
}
