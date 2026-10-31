package business

import (
	"testing"

	"github.com/livekit/protocol/livekit"
)

// CALL-BUG-16. A failed egress (out of memory, storage refused the upload)
// still sends egress_ended, and was stamped like a good recording: listed at
// full length, playing a black frame. Its status is kept.
//
// A recording that captured nothing is empty, not failed: LiveKit aborts an
// egress whose page never had a picture or a sound to start on (the demo's
// three on 28 Oct were all "EGRESS_ABORTED: Start signal not received"), and
// one that says complete with nothing written holds nothing either.
func TestStatusFromEgress(t *testing.T) {
	for _, c := range []struct {
		status livekit.EgressStatus
		size   int64
		want   string
	}{
		{livekit.EgressStatus_EGRESS_COMPLETE, 1 << 20, StatusReady},
		{livekit.EgressStatus_EGRESS_LIMIT_REACHED, 1 << 20, StatusReady},
		{livekit.EgressStatus_EGRESS_FAILED, 1 << 20, StatusFailed},
		{livekit.EgressStatus_EGRESS_FAILED, 0, StatusFailed},
		{livekit.EgressStatus_EGRESS_ABORTED, 0, StatusEmpty},
		{livekit.EgressStatus_EGRESS_COMPLETE, 0, StatusEmpty},
		{livekit.EgressStatus_EGRESS_LIMIT_REACHED, 0, StatusEmpty},
	} {
		got := StatusFromEgress(c.status, c.size)
		if got != c.want {
			t.Errorf("%s, %d bytes: %q, want %q", c.status, c.size, got, c.want)
		}
		if Failed(got) != (c.want == StatusFailed) {
			t.Errorf("Failed(%q) is wrong", got)
		}
		if Empty(got) != (c.want == StatusEmpty) {
			t.Errorf("Empty(%q) is wrong", got)
		}
	}
	if Failed("") || Empty("") {
		t.Error("a recording from before statuses were kept must still play")
	}
}

// A recording's length, from its egress's end: its file's, else from the
// egress's start to its end, only when LiveKit gave it a start. One that
// captured nothing is 0 s long, however long it waited.
func TestEndOfEgress(t *testing.T) {
	const s = int64(1e9)
	start := int64(1_800_000_000) * s
	for _, c := range []struct {
		name    string
		info    *livekit.EgressInfo
		status  string
		seconds float64
		size    int64
	}{
		{"its file's length", &livekit.EgressInfo{Status: livekit.EgressStatus_EGRESS_COMPLETE, StartedAt: start, EndedAt: start + 30*s,
			FileResults: []*livekit.FileInfo{{Size: 1 << 20, Duration: 28 * s}}}, StatusReady, 28, 1 << 20},
		{"a file without a length: start to end", &livekit.EgressInfo{Status: livekit.EgressStatus_EGRESS_COMPLETE, StartedAt: start, EndedAt: start + 30*s,
			FileResults: []*livekit.FileInfo{nil, {Size: 1 << 20}}}, StatusReady, 30, 1 << 20},
		{"the demo's shape: aborted, with a start", &livekit.EgressInfo{Status: livekit.EgressStatus_EGRESS_ABORTED, Error: livekit.MsgStartNotReceived,
			StartedAt: start, EndedAt: start + 240*s}, StatusEmpty, 0, 0},
		{"aborted before it started", &livekit.EgressInfo{Status: livekit.EgressStatus_EGRESS_ABORTED, Error: livekit.MsgStoppedBeforeStarted,
			EndedAt: start}, StatusEmpty, 0, 0},
		{"complete with nothing written", &livekit.EgressInfo{Status: livekit.EgressStatus_EGRESS_COMPLETE, StartedAt: start, EndedAt: start + 5*s,
			FileResults: []*livekit.FileInfo{{}}}, StatusEmpty, 0, 0},
		{"failed after a start", &livekit.EgressInfo{Status: livekit.EgressStatus_EGRESS_FAILED, StartedAt: start, EndedAt: start + 12*s}, StatusFailed, 12, 0},
		{"failed with no start: not decades", &livekit.EgressInfo{Status: livekit.EgressStatus_EGRESS_FAILED, EndedAt: start}, StatusFailed, 0, 0},
	} {
		status, seconds, size := EndOfEgress(c.info)
		if status != c.status || seconds != c.seconds || size != c.size {
			t.Errorf("%s: %q, %v s, %d bytes; want %q, %v s, %d bytes", c.name, status, seconds, size, c.status, c.seconds, c.size)
		}
	}
}
