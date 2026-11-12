package helpers

import (
	"testing"
	"time"
)

// Now is the real clock unless the integration build's SetClockForTest has
// set one; the clock it is set to is what it answers, until put back.
func TestNowIsTheRealClockUnlessATestSetsOne(t *testing.T) {
	before := time.Now()
	got := Now()
	if got.Before(before) || got.After(time.Now()) {
		t.Fatalf("Now() = %v, not the real clock (%v)", got, before)
	}
	pinned := time.Date(2026, 11, 8, 22, 51, 0, 0, time.UTC)
	f := func() time.Time { return pinned }
	was := clock.Swap(&f)
	t.Cleanup(func() { clock.Store(was) })
	if got := Now(); !got.Equal(pinned) {
		t.Fatalf("Now() with a clock set = %v, want %v", got, pinned)
	}
}
