package helpers

import (
	"net/http"
	"testing"
	"time"
)

// A server's Retry-After is seconds or an HTTP date; anything else, or a time
// already past, is the caller's own wait, and a huge one is capped so one
// answer can't park a job for days.
func TestRetryAfterReadsSecondsOrADate(t *testing.T) {
	const fallback = 30 * time.Second
	soon := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	for _, c := range []struct {
		header   string
		min, max time.Duration
	}{
		{"", fallback, fallback},
		{"7", 7 * time.Second, 7 * time.Second},
		{" 12 ", 12 * time.Second, 12 * time.Second},
		{"0", fallback, fallback},
		{"-4", fallback, fallback},
		{"soon", fallback, fallback},
		{soon, 80 * time.Second, 90 * time.Second},
		{past, fallback, fallback},
		{"99999999", maxRetryAfter, maxRetryAfter},
	} {
		h := http.Header{}
		if c.header != "" {
			h.Set("Retry-After", c.header)
		}
		if got := RetryAfter(h, fallback); got < c.min || got > c.max {
			t.Errorf("Retry-After %q: %v, want %v to %v", c.header, got, c.min, c.max)
		}
	}
	if got := RetryAfter(http.Header{}, 0); got != 0 {
		t.Errorf("no Retry-After and no fallback: %v", got)
	}
}
