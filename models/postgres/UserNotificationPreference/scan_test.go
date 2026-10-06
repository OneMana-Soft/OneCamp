package models

import (
	"strings"
	"testing"
	"time"
)

// countingScanner records how many targets a scan asks for.
type countingScanner struct{ n int }

func (c *countingScanner) Scan(dest ...any) error { c.n = len(dest); return nil }

// The scanner must ask for exactly one target per column in allColumns, or
// every query that selects allColumns fails at run time.
func TestScanRowMatchesColumns(t *testing.T) {
	cols := len(strings.Split(allColumns, ","))
	var c countingScanner
	if _, err := scanRow(&c); err != nil {
		t.Fatal(err)
	}
	if c.n != cols {
		t.Fatalf("scanRow reads %d targets, allColumns has %d", c.n, cols)
	}
}

// Notifications stay quiet until the later of a pause and focus time, and a
// past one of either counts for nothing.
func TestQuietUntilTakesTheLaterOfPauseAndFocus(t *testing.T) {
	now := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	at := func(h int) *time.Time { v := now.Add(time.Duration(h) * time.Hour); return &v }
	cases := []struct {
		name         string
		pause, focus *time.Time
		want         *time.Time
	}{
		{"neither", nil, nil, nil},
		{"pause only", at(1), nil, at(1)},
		{"focus only", nil, at(2), at(2)},
		{"focus outlasts the pause", at(1), at(2), at(2)},
		{"pause outlasts focus", at(3), at(2), at(3)},
		{"an expired pause is ignored", at(-1), at(2), at(2)},
		{"both over", at(-1), at(-2), nil},
	}
	for _, c := range cases {
		p := &UserNotificationPreference{NotificationsPausedUntil: c.pause, FocusUntil: c.focus}
		got := p.QuietUntil(now)
		if (got == nil) != (c.want == nil) || (got != nil && !got.Equal(*c.want)) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
