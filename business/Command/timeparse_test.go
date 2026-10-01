package business

import (
	"strings"
	"testing"
	"time"
)

func TestParseWhen_RelativeDurations(t *testing.T) {
	cases := []struct {
		phrase  string
		minWait time.Duration
		maxWait time.Duration
	}{
		{"in 20 minutes", 19 * time.Minute, 21 * time.Minute},
		{"in 2 hours", 119 * time.Minute, 121 * time.Minute},
		{"in 1 day", 23 * time.Hour, 25 * time.Hour},
	}
	for _, c := range cases {
		got, err := ParseWhen(c.phrase, "UTC")
		if err != nil {
			t.Fatalf("ParseWhen(%q) error: %v", c.phrase, err)
		}
		wait := time.Until(got.At)
		if wait < c.minWait || wait > c.maxWait {
			t.Errorf("ParseWhen(%q) fires in %v, want between %v and %v", c.phrase, wait, c.minWait, c.maxWait)
		}
		if got.Recurrence != "" {
			t.Errorf("ParseWhen(%q) should be one-shot, got recurrence %q", c.phrase, got.Recurrence)
		}
	}
}

func TestParseWhen_Recurring(t *testing.T) {
	got, err := ParseWhen("every monday at 10:00", "UTC")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(got.Recurrence, "FREQ=WEEKLY") || !strings.Contains(got.Recurrence, "MO") {
		t.Errorf("expected weekly Monday recurrence, got %q", got.Recurrence)
	}
	if got.At.Weekday() != time.Monday {
		t.Errorf("first fire should be a Monday, got %v", got.At.Weekday())
	}
	if got.At.Hour() != 10 {
		t.Errorf("expected hour 10, got %d", got.At.Hour())
	}
}

func TestParseWhen_EveryDay(t *testing.T) {
	got, err := ParseWhen("every day at 9am", "UTC")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if got.Recurrence != "FREQ=DAILY" {
		t.Errorf("expected FREQ=DAILY, got %q", got.Recurrence)
	}
	if got.At.Hour() != 9 {
		t.Errorf("expected hour 9, got %d", got.At.Hour())
	}
}

func TestParseWhen_TomorrowAt(t *testing.T) {
	got, err := ParseWhen("tomorrow at 3pm", "UTC")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if got.At.Hour() != 15 {
		t.Errorf("expected hour 15 (3pm), got %d", got.At.Hour())
	}
	if got.At.Before(time.Now()) {
		t.Errorf("tomorrow should be in the future, got %v", got.At)
	}
}

func TestParseWhen_Invalid(t *testing.T) {
	if _, err := ParseWhen("sometime soonish", "UTC"); err == nil {
		t.Errorf("expected error for unparseable phrase")
	}
}
