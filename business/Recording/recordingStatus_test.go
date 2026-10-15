package business

import (
	"testing"

	"github.com/livekit/protocol/livekit"
)

// CALL-BUG-16. A failed or aborted egress (out of memory, the room closed
// under it, storage refused the upload) still sends egress_ended, and was
// stamped like a good recording: listed at full length, playing a black
// frame. Its status is kept, and a complete egress that wrote nothing is a
// failure too.
func TestStatusFromEgress(t *testing.T) {
	for _, c := range []struct {
		status livekit.EgressStatus
		size   int64
		want   string
	}{
		{livekit.EgressStatus_EGRESS_COMPLETE, 1 << 20, StatusReady},
		{livekit.EgressStatus_EGRESS_LIMIT_REACHED, 1 << 20, StatusReady},
		{livekit.EgressStatus_EGRESS_FAILED, 1 << 20, StatusFailed},
		{livekit.EgressStatus_EGRESS_ABORTED, 0, StatusFailed},
		{livekit.EgressStatus_EGRESS_COMPLETE, 0, StatusFailed},
	} {
		got := StatusFromEgress(c.status, c.size)
		if got != c.want {
			t.Errorf("%s, %d bytes: %q, want %q", c.status, c.size, got, c.want)
		}
		if Failed(got) != (c.want == StatusFailed) {
			t.Errorf("Failed(%q) is wrong", got)
		}
	}
	if Failed("") {
		t.Error("a recording from before statuses were kept must still play")
	}
}
