package business

import (
	"strings"
	"testing"
	"time"
)

func TestValidateSendAt(t *testing.T) {
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		at time.Time
		ok bool
	}{
		{now.Add(10 * time.Second), false},     // too soon: it would just be "send"
		{now.Add(2 * time.Minute), true},       // a couple of minutes out
		{now.Add(119 * 24 * time.Hour), true},  // within the window
		{now.Add(121 * 24 * time.Hour), false}, // past it
		{now.Add(-time.Hour), false},           // the past
	}
	for _, c := range cases {
		if err := ValidateSendAt(c.at, now); (err == nil) != c.ok {
			t.Errorf("ValidateSendAt(%v) err=%v, want ok=%v", c.at.Sub(now), err, c.ok)
		}
	}
	if _, isInvalid := ValidateSendAt(now, now).(*Invalid); !isInvalid {
		t.Error("a bad time must be an Invalid, so the person sees why")
	}
	// What it says is the rule it applies: 31 seconds out passes, so it never
	// says "a minute" (MC-41).
	if err := ValidateSendAt(now.Add(31*time.Second), now); err != nil {
		t.Errorf("31 seconds out was refused: %v", err)
	}
	if err := ValidateSendAt(now.Add(10*time.Second), now); err == nil || err.Error() != "Pick a time at least 30 seconds from now." {
		t.Errorf("too soon says %v", err)
	}
}

func TestPreview(t *testing.T) {
	if got := Preview("<p>Ship it <strong>Tuesday</strong></p><p>  please </p>"); got != "Ship it Tuesday please" {
		t.Errorf("Preview flattened to %q", got)
	}
	long := "<p>" + strings.Repeat("word ", 60) + "</p>"
	got := Preview(long)
	if n := len([]rune(got)); n != 140 || !strings.HasSuffix(got, "…") {
		t.Errorf("long preview is %d runes, %q", n, got[len(got)-5:])
	}
}
