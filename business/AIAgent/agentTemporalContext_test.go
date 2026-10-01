package business

import (
	"strings"
	"testing"
	"time"
)

func TestBuildTemporalContext_IncludesDateAndGuidance(t *testing.T) {
	now := time.Date(2026, time.July, 2, 13, 45, 0, 0, time.UTC)
	got := buildTemporalContext(now)

	for _, want := range []string{
		"Thursday",      // weekday of 2026-07-02
		"02 Jul 2026",   // date
		"13:45",         // time
		"UTC",           // zone label
		"today",         // relative-time guidance
		"training data", // must tell the model not to trust its own clock
	} {
		if !strings.Contains(got, want) {
			t.Errorf("temporal context missing %q, got:\n%s", want, got)
		}
	}
}

func TestBuildTemporalContext_NormalisesToUTC(t *testing.T) {
	// A non-UTC input must still render as UTC (stable regardless of server tz).
	loc := time.FixedZone("IST", 5*3600+1800) // +05:30
	now := time.Date(2026, time.July, 2, 19, 15, 0, 0, loc)
	got := buildTemporalContext(now)
	if !strings.Contains(got, "13:45") { // 19:15 +05:30 -> 13:45 UTC
		t.Errorf("expected UTC-normalised time 13:45, got:\n%s", got)
	}
}
