package helpers

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseSeatLimit(t *testing.T) {
	for in, want := range map[string]int{"": 0, "25": 25, " 10 ": 10, "0": 0, "-3": 0, "x": 0} {
		if got := parseSeatLimit(in); got != want {
			t.Errorf("parseSeatLimit(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestSeatLimitError(t *testing.T) {
	err := fmt.Errorf("creating user: %w", &SeatLimitError{Limit: 25})
	if !IsSeatLimit(err) {
		t.Fatal("a wrapped seat limit must still be recognised")
	}
	if IsSeatLimit(fmt.Errorf("other")) {
		t.Fatal("other errors are not seat limits")
	}
	msg := (&SeatLimitError{Limit: 25}).Error()
	for _, want := range []string{"25 people", "deactivate", "onemana.dev/pricing"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}
