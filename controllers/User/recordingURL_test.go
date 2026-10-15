package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// CALL-BUG-15. A DM's recording opened through a channel's route (or the
// reverse) dereferenced a nil channel and the request died with a 500. It is
// a 404 now; somebody else's is a 403.
func TestARecordingOpenedByTheWrongRouteIsNotFound(t *testing.T) {
	dm := &dgraphStruct.DgraphRecording{Dm: &dgraphStruct.DgraphDm{GroupingId: "g1", ParticipantIsMember: 1}}
	ch := &dgraphStruct.DgraphRecording{Channel: &dgraphStruct.DgraphChannel{Uuid: "c1", IsMember: 1}}
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"a DM's recording by the channel route", channelRecordingAccess(dm, "c1"), http.StatusNotFound},
		{"a channel's recording by the DM route", chatRecordingAccess(ch, "g1"), http.StatusNotFound},
		{"none, by the channel route", channelRecordingAccess(nil, "c1"), http.StatusNotFound},
		{"none, by the DM route", chatRecordingAccess(nil, "g1"), http.StatusNotFound},
		{"another channel's", channelRecordingAccess(ch, "c2"), http.StatusForbidden},
		{"not a member", channelRecordingAccess(&dgraphStruct.DgraphRecording{Channel: &dgraphStruct.DgraphChannel{Uuid: "c1"}}, "c1"), http.StatusForbidden},
		{"another conversation's", chatRecordingAccess(dm, "g2"), http.StatusForbidden},
		{"the channel's own", channelRecordingAccess(ch, "c1"), 0},
		{"the DM's own", chatRecordingAccess(dm, "g1"), 0},
	} {
		if c.got != c.want {
			t.Errorf("%s: %d, want %d", c.name, c.got, c.want)
		}
	}
}

// What the player is told when there is nothing to play: still being saved,
// failed, or a call's transcript with no recording (CALL-BUG-16, CALL-BUG-3).
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
	} {
		w := httptest.NewRecorder()
		writeRecordingURL(w, httptest.NewRequest(http.MethodGet, "/", nil), c.rec)
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
