package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bodyLimit_test.go — the cap has to do two things: refuse an oversized request
// with an answer the caller can act on, and never interfere with a normal one.
//
// The reason the status code matters: previously only MaxBytesReader was applied, so
// an oversized body reached the handler and failed as a generic decode error. A
// client could not distinguish "too large" from "malformed", which is why the
// frontend's upload path had to guess.

func okHandler() (http.Handler, *bool) {
	reached := false
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	return h, &reached
}

func TestBodyLimit_RefusesADeclaredOversizedBodyWith413(t *testing.T) {
	h, reached := okHandler()
	req := httptest.NewRequest(http.MethodPost, "/doc/update", strings.NewReader(strings.Repeat("x", 100)))
	req.ContentLength = 50 << 20 // client says 50 MB
	rec := httptest.NewRecorder()

	BodyLimit(8<<20)(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized body must be refused with 413, got %d", rec.Code)
	}
	if *reached {
		t.Fatal("the handler must not run for a request that was already refused")
	}
	// The answer must name the limit, or the caller cannot react correctly.
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal must be JSON the client can parse: %v", rec.Body.String())
	}
	if body["max_bytes"] == nil {
		t.Fatalf("the refusal must state the limit; got %v", body)
	}
}

func TestBodyLimit_AllowsANormalRequest(t *testing.T) {
	h, reached := okHandler()
	req := httptest.NewRequest(http.MethodPost, "/doc/update", strings.NewReader(strings.Repeat("x", 1024)))
	rec := httptest.NewRecorder()

	BodyLimit(8<<20)(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !*reached {
		t.Fatalf("a normal request must pass through; code=%d reached=%v", rec.Code, *reached)
	}
}

// A body with no declared length (chunked, or a lying client) must still be capped —
// that is what MaxBytesReader is for, and it is why the early check is not enough
// on its own.
func TestBodyLimit_CapsAnUndeclaredOversizedBody(t *testing.T) {
	var readErr error
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	})
	req := httptest.NewRequest(http.MethodPost, "/doc/update", strings.NewReader(strings.Repeat("x", 4096)))
	req.ContentLength = -1 // unknown length
	rec := httptest.NewRecorder()

	BodyLimit(1024)(h).ServeHTTP(rec, req)

	if readErr == nil {
		t.Fatal("reading past the cap must fail, or an unbounded body could still be buffered")
	}
}

// A misconfigured (non-positive) cap must not take a route offline.
func TestBodyLimit_NonPositiveCapIsAPassThrough(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		h, reached := okHandler()
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("hello"))
		req.ContentLength = 99 << 20
		rec := httptest.NewRecorder()

		BodyLimit(limit)(h).ServeHTTP(rec, req)

		if !*reached || rec.Code != http.StatusOK {
			t.Fatalf("limit %d must disable the cap, not reject everything; code=%d reached=%v",
				limit, rec.Code, *reached)
		}
	}
}

// A GET with no body must be untouched.
func TestBodyLimit_LeavesBodylessRequestsAlone(t *testing.T) {
	h, reached := okHandler()
	req := httptest.NewRequest(http.MethodGet, "/doc/list", nil)
	rec := httptest.NewRecorder()

	BodyLimit(8<<20)(h).ServeHTTP(rec, req)

	if !*reached || rec.Code != http.StatusOK {
		t.Fatalf("a bodyless request must pass; code=%d reached=%v", rec.Code, *reached)
	}
}
