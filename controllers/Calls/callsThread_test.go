package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	callThreadBusiness "github.com/akashc777/OneCamp/business/CallThread"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	"github.com/google/uuid"
)

// A reply through the call's chat answers in the writer's words: what's wrong
// with their text, a chat they may only read, one that's gone, or a retry.
func TestACallThreadReplyAnswersInTheWritersWords(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
		want string
	}{
		{nil, http.StatusOK, "Sent"},
		{&guestBusiness.ErrGuestInput{Msg: "Write a message first."}, http.StatusBadRequest, "Write a message first."},
		{callThreadBusiness.ErrReadOnly, http.StatusForbidden, "You can read this call's chat, but not write in it."},
		{callThreadBusiness.ErrNotFound, http.StatusNotFound, "This call's chat isn't here. The call may have ended."},
		{guestBusiness.ErrNotFound, http.StatusNotFound, "This call's chat isn't here. The call may have ended."},
		{errors.New("dgraph down"), http.StatusServiceUnavailable, "Your message couldn't be sent just now. Try again in a moment."},
	} {
		w := httptest.NewRecorder()
		answerCallThreadReply(w, httptest.NewRequest(http.MethodPost, "/calls/s1/thread", nil), c.err)
		var out struct {
			Msg string `json:"msg"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != c.code || out.Msg != c.want {
			t.Errorf("%v: %d %q", c.err, w.Code, out.Msg)
		}
	}
}

// Nobody writes in a call's chat through an API token, and a body that isn't
// one is said so before anything is asked.
func TestACallThreadReplyIsTheAppsOnly(t *testing.T) {
	maya := uuid.New()
	if code, out := roomAsk(ReplyInCallThread, http.MethodPost, `{"text":"hi"}`, &maya, true); code != http.StatusNotFound || codeOf(out) != "not_found" {
		t.Errorf("through a token: %d %s", code, out)
	}
	if code, _ := roomAsk(ReplyInCallThread, http.MethodPost, `{"text":`, &maya, false); code != http.StatusBadRequest {
		t.Errorf("a broken body: %d", code)
	}
	if code, _ := roomAsk(GuestReplyInCallThread, http.MethodPost, `[]`, nil, false); code != http.StatusBadRequest {
		t.Errorf("a guest's broken body: %d", code)
	}
}

// A poll with nothing new carries nothing: 204 and no body. Its since is read
// as RFC 3339; anything else asks for the whole thread.
func TestAnUnchangedPollCarriesNothing(t *testing.T) {
	w := httptest.NewRecorder()
	answerCallThread(w, httptest.NewRequest(http.MethodGet, "/guest/calls/thread", nil), nil, callThreadBusiness.ErrUnchanged)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Errorf("an unchanged poll: %d %q", w.Code, w.Body.String())
	}
	at := time.Date(2026, 10, 22, 9, 30, 0, 123456789, time.UTC)
	if got := pollSince(httptest.NewRequest(http.MethodGet, "/guest/calls/thread?since="+url.QueryEscape(at.Format(time.RFC3339Nano)), nil)); !got.Equal(at) {
		t.Errorf("since %v, want %v", got, at)
	}
	for _, q := range []string{"", "?since=yesterday"} {
		if got := pollSince(httptest.NewRequest(http.MethodGet, "/guest/calls/thread"+q, nil)); !got.IsZero() {
			t.Errorf("%q read as %v", q, got)
		}
	}
}
