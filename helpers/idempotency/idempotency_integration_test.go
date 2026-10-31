//go:build integration

package idempotency

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A write whose handler panics stored no answer, so its key is let go as for
// a 5xx: a retry under the same key runs the write again, instead of being
// told for the key's 24 hours that it is still on its way (409).
func TestAKeyIsLetGoWhenItsWritePanics(t *testing.T) {
	integration.SetupRedis(t)
	runs := 0
	h := Middleware("POST /test", InWords)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		runs++
		if runs == 1 {
			panic("the write failed half way")
		}
		w.WriteHeader(http.StatusCreated)
	}))
	send := func() (code int, panicked bool) {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"n":1}`)))
		r.Header.Set("Idempotency-Key", "k-panic")
		r = r.WithContext(context.WithValue(r.Context(), helpers.ApiTokenIDContextKey, "tok-1"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, false
	}

	if _, panicked := send(); !panicked {
		t.Fatal("the first send didn't panic: the test proves nothing")
	}
	if code, panicked := send(); panicked || code != http.StatusCreated || runs != 2 {
		t.Errorf("the retry answered %d (panicked %v) after %d runs, want 201 from a second run", code, panicked, runs)
	}
	// The second answer is kept: the same send again is replayed, not run.
	if code, _ := send(); code != http.StatusCreated || runs != 2 {
		t.Errorf("a third send answered %d after %d runs, want the kept 201 and no run", code, runs)
	}
}

// A write that was done keeps its key when Redis couldn't store its answer: a
// retry under the key is refused as still on its way, never run a second time
// (a second doc, a second task), which is the one thing the key exists to stop.
func TestADoneWriteKeepsItsKeyWhenItsAnswerIsLost(t *testing.T) {
	integration.SetupRedis(t)
	storeAnswer = func(context.Context, registry.Spec, []string, string, time.Duration) error {
		return errors.New("redis refused the answer")
	}
	t.Cleanup(func() { storeAnswer = redisStore.SetStringWithTTL })
	runs := 0
	h := Middleware("POST /test", InWords)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		runs++
		w.WriteHeader(http.StatusCreated)
	}))
	send := func() int {
		r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"n":1}`)))
		r.Header.Set("Idempotency-Key", "k-lost")
		r = r.WithContext(context.WithValue(r.Context(), helpers.ApiTokenIDContextKey, "tok-1"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	if code := send(); code != http.StatusCreated {
		t.Fatalf("the first send answered %d, want 201", code)
	}
	if code := send(); code != http.StatusConflict || runs != 1 {
		t.Errorf("the retry answered %d after %d runs, want 409 with the write run once", code, runs)
	}
}
