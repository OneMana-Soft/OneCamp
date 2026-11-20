package helpers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A refusal answers 403 with its own code and words, even when it was
// wrapped on its way up, and anything else is left to the caller.
func TestACallRefusalIsAnsweredInItsOwnWords(t *testing.T) {
	locked := &CallRefusal{Code: "call_locked", Msg: "This call is locked."}
	rec := httptest.NewRecorder()
	if !WriteCallRefusal(rec, fmt.Errorf("minting a token: %w", locked)) {
		t.Fatal("a wrapped refusal wasn't answered")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "call_locked" || body["msg"] != "This call is locked." {
		t.Fatalf("answered %v", body)
	}

	other := httptest.NewRecorder()
	if WriteCallRefusal(other, errors.New("the call server is down")) {
		t.Fatal("an ordinary error was answered as a refusal")
	}
	if other.Body.Len() != 0 {
		t.Fatalf("an ordinary error wrote %q", other.Body.String())
	}
}

// A refusal that passes (the call couldn't be checked just now) is answered
// with its own status, a 503 a page tries again, in its own words.
func TestARefusalThatPassesIsAnsweredToTryAgain(t *testing.T) {
	unchecked := &CallRefusal{Code: "call_unchecked", Msg: "We couldn't check the call just now. Try again in a moment.", Status: http.StatusServiceUnavailable}
	rec := httptest.NewRecorder()
	if !WriteCallRefusal(rec, fmt.Errorf("minting a token: %w", unchecked)) || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("answered %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["code"] != "call_unchecked" {
		t.Fatalf("answered %v %v", body, err)
	}
}

// V-1281: RefusedUnless splits a check's read. A yes passes; a no, and a read
// that found nothing (ErrNotFound, as a model's NotFound makes it), are the
// refusal itself; a read that failed still refuses (never nil), in the
// refusal's words, but is no Refusal, and its cause is logged at ERROR there.
func TestRefusedUnlessSplitsAFailedReadFromANo(t *testing.T) {
	ctx := context.Background()
	refusal := &Refusal{Status: http.StatusBadRequest, Msg: "choose a channel you can read"}
	for _, c := range []struct {
		name string
		ok   bool
		err  error
		want error
	}{
		{"a yes", true, nil, nil},
		{"a no", false, nil, refusal},
		{"nothing by that id", false, ErrNotFound, refusal},
		{"a model's own not-found, wrapped", false, fmt.Errorf("reading: %w", NotFound("channel not found in dgraph")), refusal},
	} {
		h := withCapturedLogger(t)
		if got := RefusedUnless(ctx, "test check", c.ok, c.err, refusal); got != c.want {
			t.Errorf("%s: got %#v, want %#v", c.name, got, c.want)
		}
		for _, r := range h.records {
			if r.Level >= slog.LevelError {
				t.Errorf("%s: logged an error: %q", c.name, r.Message)
			}
		}
	}

	for _, ok := range []bool{false, true} {
		h := withCapturedLogger(t)
		got := RefusedUnless(ctx, "business/Test check", ok, errors.New("graph unreachable"), refusal)
		if got == nil {
			t.Fatalf("ok=%v: a read that failed passed", ok)
		}
		var refused *Refusal
		if errors.As(got, &refused) || errors.Is(got, refusal) || errors.Is(got, ErrNotFound) {
			t.Errorf("ok=%v: a read that failed answered as %#v, want no Refusal", ok, got)
		}
		if got.Error() != refusal.Msg {
			t.Errorf("ok=%v: words %q, want %q", ok, got.Error(), refusal.Msg)
		}
		if errors.Unwrap(got) != nil {
			t.Errorf("ok=%v: carries its cause to an answer", ok)
		}
		if len(h.records) != 1 || h.records[0].Level != slog.LevelError ||
			!strings.Contains(h.records[0].Message, "business/Test check") || !strings.Contains(h.records[0].Message, "graph unreachable") {
			t.Errorf("ok=%v: the cause isn't logged at the check: %d records", ok, len(h.records))
		}
	}
}
