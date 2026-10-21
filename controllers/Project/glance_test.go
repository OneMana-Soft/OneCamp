package controllers

import (
	"testing"
	"time"
)

// The glance's day is the reader's: 01:00 in Kolkata is still yesterday in
// UTC, and today began at local midnight.
func TestTheGlanceCountsInTheReadersDay(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	now := time.Date(2026, 10, 18, 19, 30, 0, 0, time.UTC) // 01:00 on the 19th in India
	got := startOfDay(now, ist)
	if !got.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, ist)) {
		t.Fatalf("today began at %v", got)
	}
}
