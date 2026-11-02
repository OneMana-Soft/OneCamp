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

// A log line names a request by the route it matched, through a mounted
// router too, and never by its path: one that matched nothing (yet) is
// "(no route)".
func TestRouteOfNamesTheRouteNotThePath(t *testing.T) {
	var named []string
	name := func(_ http.ResponseWriter, r *http.Request) { named = append(named, RouteOf(r)) }
	v1 := chi.NewRouter()
	v1.Get("/docs/{doc_uuid}", name)
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name(w, r) // before routing
			next.ServeHTTP(w, r)
		})
	})
	router.Get("/guest/m/{token}", name)
	router.Mount("/v1", v1)

	for _, path := range []string{"/guest/m/a-link-token", "/v1/docs/7c9e6679-7425-40de-944b-e07fc1f90ae7", "/nothing/here"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	want := []string{"(no route)", "/guest/m/{token}", "(no route)", "/v1/docs/{doc_uuid}", "(no route)"}
	if len(named) != len(want) {
		t.Fatalf("named %q, want %q", named, want)
	}
	for i := range want {
		if named[i] != want[i] {
			t.Errorf("named %q, want %q", named, want)
			break
		}
	}
	outside := httptest.NewRequest(http.MethodGet, "/guest/m/a-link-token", nil).WithContext(context.Background())
	if got := RouteOf(outside); got != "(no route)" {
		t.Errorf("a request no router saw is %q, want (no route)", got)
	}
}

// ⌘K sends its words through encodeURIComponent, and chi hands a parameter
// back escaped whenever the request's escaping differs from Go's own.
func TestPathTextIsTheWordsAsTyped(t *testing.T) {
	r := chi.NewRouter()
	var got string
	r.Route("/search", func(r chi.Router) {
		r.Get("/unifiedSearch/{search_text}", func(w http.ResponseWriter, r *http.Request) { got = PathText(r, "search_text") })
	})
	for sent, want := range map[string]string{
		"type%3Atask%20launch": "type:task launch", // ":" as %3A: chi reads the raw path
		"Q%26A":                "Q&A",
		"C%2B%2B":              "C++",
		"from%3A%40alice":      "from:@alice",
		"launch%20plan":        "launch plan", // Go's own escaping: chi reads the decoded path
		"50%25%20off":          "50% off",
		"%2526":                "%26", // a typed "%26": decoded once, never twice
		"a%3A%2526":            "a:%26",
	} {
		got = ""
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/search/unifiedSearch/"+sent, nil))
		if got != want {
			t.Errorf("%s reads %q; want %q", sent, got, want)
		}
	}
}
