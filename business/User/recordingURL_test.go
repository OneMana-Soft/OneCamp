package business

import (
	"testing"
	"time"
)

// CALL-BUG-10. The player's link lived five minutes and was fetched once, so a
// long recording stopped when the browser asked for its next range after that,
// and every seek after five minutes failed.
func TestARecordingLinkOutlivesItsRecording(t *testing.T) {
	for _, c := range []struct {
		seconds float64
		want    time.Duration
	}{
		{0, 30 * time.Minute},
		{-5, 30 * time.Minute},
		{3600, 90 * time.Minute},
		{100 * 3600, 12 * time.Hour},
	} {
		if got := RecordingURLTTL(c.seconds); got != c.want {
			t.Errorf("a %vs recording: link lives %s, want %s", c.seconds, got, c.want)
		}
	}
}
