package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// zoom-09 C2: a save that would send call audio off the server is refused,
// before anything is stored, unless it carries the admin's acknowledgement of
// exactly where it goes; the refusal names that place for the card to show.
func TestASaveSendingAudioAwayNeedsTheAcknowledgement(t *testing.T) {
	for _, k := range []string{"TRANSCRIPTION_MODE", "STT_PROVIDER", "STT_BASE_URL"} {
		t.Setenv(k, "")
	}
	for _, c := range []struct{ body, where string }{
		{`{"mode":"frontend"}`, "browser"},
		{`{"mode":"frontend","acknowledge_where":"deepgram"}`, "browser"},
		{`{"mode":"backend","stt_provider":"deepgram"}`, "deepgram"},
		{`{"stt_provider":"google"}`, "google"},
	} {
		w := httptest.NewRecorder()
		UpdateConfig(w, httptest.NewRequest(http.MethodPost, "/admin/transcription/config", strings.NewReader(c.body)))
		var got struct {
			Code  string `json:"code"`
			Where string `json:"where"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != http.StatusConflict || got.Code != "acknowledgement_required" || got.Where != c.where {
			t.Errorf("%s: %d %+v, want 409 acknowledgement_required naming %s", c.body, w.Code, got, c.where)
		}
	}
}

// zoom-09 C2 F2: no record, no action. An acknowledgement the audit log can't
// take stops the save: 503, and nothing is stored.
func TestAnAcknowledgementThatCannotBeRecordedSavesNothing(t *testing.T) {
	for _, k := range []string{"TRANSCRIPTION_MODE", "STT_PROVIDER", "STT_BASE_URL"} {
		t.Setenv(k, "")
	}
	written := 0
	was := recordCallPrivacy
	t.Cleanup(func() { recordCallPrivacy = was })
	recordCallPrivacy = func(*http.Request, string, string, map[string]interface{}) error {
		written++
		return errors.New("the audit log is down")
	}
	// A unit test has no database: a Save that went on would panic on it.
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("the save went on without its audit row: %v", p)
		}
	}()
	w := httptest.NewRecorder()
	UpdateConfig(w, httptest.NewRequest(http.MethodPost, "/admin/transcription/config",
		strings.NewReader(`{"mode":"frontend","acknowledge_where":"browser"}`)))
	if written != 1 || w.Code != http.StatusServiceUnavailable {
		t.Errorf("an unrecorded acknowledgement: %d writes, answer %d, want 1 and 503", written, w.Code)
	}
}
