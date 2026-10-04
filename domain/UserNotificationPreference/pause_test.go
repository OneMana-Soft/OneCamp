package domain

import (
	"errors"
	"testing"
	"time"
)

func TestCheckPauseEnd(t *testing.T) {
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	var pe *PauseError
	if err := CheckPauseEnd(now.Add(time.Hour), now); err != nil {
		t.Fatalf("an hour: %v", err)
	}
	if err := CheckPauseEnd(now.Add(MaxPause), now); err != nil {
		t.Fatalf("exactly a week: %v", err)
	}
	for _, until := range []time.Time{now, now.Add(-time.Minute), now.Add(MaxPause + time.Second)} {
		if err := CheckPauseEnd(until, now); !errors.As(err, &pe) {
			t.Errorf("%v: want a PauseError, got %v", until, err)
		}
	}
}
