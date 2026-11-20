package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// What the player is told when there is nothing to play: still being saved,
// failed, nothing captured (V-32), or a call's transcript with no recording
// (CALL-BUG-16, CALL-BUG-3).
// None of these carries a link.
func TestARecordingWithNothingToPlaySaysWhy(t *testing.T) {
	ended := time.Now()
	for _, c := range []struct {
		rec  *dgraphStruct.DgraphRecording
		want string
	}{
		{&dgraphStruct.DgraphRecording{TranscriptOnly: true, EndedAt: &ended}, "transcript_only"},
		{&dgraphStruct.DgraphRecording{}, "recording"},
		{&dgraphStruct.DgraphRecording{EndedAt: &time.Time{}}, "recording"},
		{&dgraphStruct.DgraphRecording{EndedAt: &ended, Status: "failed"}, "failed"},
		{&dgraphStruct.DgraphRecording{EndedAt: &ended, Status: "empty"}, "empty"},
	} {
		w := httptest.NewRecorder()
		writeRecordingLink(w, httptest.NewRequest(http.MethodGet, "/", nil), c.rec)
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusOK || got["status"] != c.want {
			t.Errorf("%+v: %d %v, want status %q", c.rec, w.Code, got, c.want)
		}
		if _, hasURL := got["url"]; hasURL {
			t.Errorf("%+v carries a link to nothing: %v", c.rec, got)
		}
	}
}

// A recording's link asks the one read rule with the room, the asker and the
// key it was asked by, and the recording's start; refused, it answers as if
// there were no recording, and carries no link and no status.
func TestARecordingLinkAsksTheOneReadRule(t *testing.T) {
	began := time.Now().Add(-time.Hour)
	ended := time.Now()
	type asked struct {
		room, identity, key string
		began               *time.Time
	}
	t.Cleanup(func(was func(context.Context, string, string, string, *time.Time) bool) func() {
		return func() { recordingOpenTo = was }
	}(recordingOpenTo))
	for _, open := range []bool{false, true} {
		var got asked
		recordingOpenTo = func(_ context.Context, room, identity, key string, b *time.Time) bool {
			got = asked{room, identity, key, b}
			return open
		}
		w := httptest.NewRecorder()
		WriteRecordingURL(w, httptest.NewRequest(http.MethodGet, "/", nil),
			&dgraphStruct.DgraphRecording{TranscriptOnly: true, StartedAt: &began, EndedAt: &ended}, "room-1", "0x1", "EG_1")
		if got != (asked{"room-1", "0x1", "EG_1", &began}) {
			t.Errorf("the rule was asked %+v", got)
		}
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		switch {
		case !open && (w.Code != http.StatusNotFound || body["msg"] != "Recording not found" || body["status"] != nil || body["url"] != nil):
			t.Errorf("refused by the rule: %d %v, want 404 Recording not found", w.Code, body)
		case open && (w.Code != http.StatusOK || body["status"] != "transcript_only"):
			t.Errorf("open by the rule: %d %v, want the recording's own answer", w.Code, body)
		}
	}
}
