package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// form writes a file as a browser sends it, with a boundary of its own.
func form(t *testing.T, name, text string) (string, []byte) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte(text))
	_ = w.Close()
	return w.FormDataContentType(), body.Bytes()
}

// A form sent again is the same request whatever its boundary; another file is
// another request; any other body is its bytes' hash, as /v1 has always
// stored it, so keys taken before this change still answer.
func TestTheSameFormSentAgainIsTheSameRequest(t *testing.T) {
	typeA, a := form(t, "plan.md", "# Plan\n")
	typeB, b := form(t, "plan.md", "# Plan\n")
	if typeA == typeB {
		t.Fatal("the two sends share a boundary: the test proves nothing")
	}
	if fingerprint(typeA, a) != fingerprint(typeB, b) {
		t.Error("the same form, sent twice, is two requests")
	}
	typeC, c := form(t, "plan.md", "# Another plan\n")
	if fingerprint(typeA, a) == fingerprint(typeC, c) {
		t.Error("another file is the same request")
	}
	body := []byte(`{"n":1}`)
	sum := sha256.Sum256(body)
	if got := fingerprint("application/json", body); got != hex.EncodeToString(sum[:]) {
		t.Errorf("a JSON body's fingerprint %s isn't its hash", got)
	}
}

// A key is the API token's when one acts, else the signed-in member's.
func TestAKeyIsItsCallers(t *testing.T) {
	member := uuid.New()
	var u userModels.UserInfo
	u.UserPostgresInfo.Id = member
	signedIn := context.WithValue(context.Background(), helpers.UserInfoContextKey, u)
	for name, c := range map[string]struct {
		ctx  context.Context
		want string
	}{
		"a token":            {context.WithValue(signedIn, helpers.ApiTokenIDContextKey, "tok-1"), "tok-1"},
		"a signed-in member": {signedIn, "member:" + member.String()},
		"nobody":             {context.Background(), ""},
	} {
		r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(c.ctx)
		if got := caller(r); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// Without Redis a write under a key runs, every time: an outage never takes a
// route down.
func TestWithoutRedisAKeyedWriteRuns(t *testing.T) {
	runs := 0
	h := Middleware("POST /test", InWords)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		runs++
		w.WriteHeader(http.StatusAccepted)
	}))
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{}`)))
		r.Header.Set("Idempotency-Key", "k-1")
		r = r.WithContext(context.WithValue(r.Context(), helpers.ApiTokenIDContextKey, "tok-1"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusAccepted {
			t.Fatalf("send %d answered %d", i+1, w.Code)
		}
	}
	if runs != 2 {
		t.Errorf("two sends ran %d times", runs)
	}
}

// The app's own routes say a refusal in words, with its status; a body over
// its route's cap is a 413.
func TestARefusalInWords(t *testing.T) {
	for _, c := range []struct {
		why  Refusal
		err  error
		code int
	}{
		{KeyTooLong, nil, http.StatusBadRequest},
		{Unreadable, fmt.Errorf("read: %w", &http.MaxBytesError{Limit: 8 << 20}), http.StatusRequestEntityTooLarge},
		{Unreadable, errors.New("unexpected EOF"), http.StatusBadRequest},
		{Reused, nil, http.StatusUnprocessableEntity},
		{InProgress, nil, http.StatusConflict},
	} {
		w := httptest.NewRecorder()
		InWords(w, c.why, c.err)
		var out map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != c.code || out["msg"] == "" {
			t.Errorf("refusal %d (%v): %d %v, want %d with words", c.why, c.err, w.Code, out, c.code)
		}
	}
}
