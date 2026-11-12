package livekitInit

import (
	"testing"

	"github.com/livekit/protocol/livekit"
)

// The recorder draws the call's own stage from the web's layout page when the
// web has an origin its Chrome can reach; otherwise LiveKit's grid, as before.
func TestRecordingLayoutURL(t *testing.T) {
	for _, c := range []struct{ name, frontend, want string }{
		{"no web host", "", ""},
		{"a host", "team.example.test", "https://team.example.test/call-recording-layout"},
		{"a trailing slash", "https://team.example.test/", "https://team.example.test/call-recording-layout"},
		{"a scheme of its own", "http://team.example.test:8080", "http://team.example.test:8080/call-recording-layout"},
		{"localhost", "localhost:3001", ""},
		{"loopback", "http://127.0.0.1:3000/", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("FRONTEND_DOMAIN", c.frontend)
			t.Setenv("FE_HOST_DOMAIN", "")
			t.Setenv("COOKIE_SECURE", "")
			if got := RecordingLayoutURL(); got != c.want {
				t.Errorf("RecordingLayoutURL() = %q, want %q", got, c.want)
			}
		})
	}
}

// IsStageRecording: only an egress that drew the stage page names it.
func TestIsStageRecording(t *testing.T) {
	stage := &livekit.EgressInfo{Request: &livekit.EgressInfo_RoomComposite{RoomComposite: &livekit.RoomCompositeEgressRequest{CustomBaseUrl: "https://team.example.test/call-recording-layout"}}}
	grid := &livekit.EgressInfo{Request: &livekit.EgressInfo_RoomComposite{RoomComposite: &livekit.RoomCompositeEgressRequest{Layout: "grid-light"}}}
	if !IsStageRecording(stage) || IsStageRecording(grid) || IsStageRecording(&livekit.EgressInfo{}) || IsStageRecording(nil) {
		t.Errorf("stage %v, grid %v, no request %v, nil %v", IsStageRecording(stage), IsStageRecording(grid), IsStageRecording(&livekit.EgressInfo{}), IsStageRecording(nil))
	}
}

// EgressReason is LiveKit's reason for the log: its first line, never the
// recorder's token, which the layout page's address carries in its query.
func TestEgressReason(t *testing.T) {
	for _, c := range []struct{ name, reason, want string }{
		{"none", "", ""},
		{"a reason", "page load failed", "page load failed"},
		{"lines", "pipeline failed\ngoroutine 1 [running]", "pipeline failed"},
		{"a page's address", "page load failed: net::ERR_NAME_NOT_RESOLVED at https://team.example.test/call-recording-layout?url=wss%3A%2F%2Flk.example.test&token=eyJhbGciOi.x.y (3 tries)",
			"page load failed: net::ERR_NAME_NOT_RESOLVED at https://team.example.test/call-recording-layout?... (3 tries)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := EgressReason(&livekit.EgressInfo{Error: c.reason}); got != c.want {
				t.Errorf("EgressReason = %q, want %q", got, c.want)
			}
		})
	}
	if got := EgressReason(nil); got != "" {
		t.Errorf("EgressReason(nil) = %q, want none", got)
	}
}
