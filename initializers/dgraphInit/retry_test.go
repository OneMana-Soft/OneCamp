package dgraphInit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/dgraph-io/dgo/v230"
)

// Each try waits between half and all of its step, and every wait together
// stays within a request's patience (3.5 s for the eight tries).
func TestAnAbortedWriteWaitsItsTurnWithJitter(t *testing.T) {
	var most time.Duration
	for attempt := 1; attempt < abortRetries; attempt++ {
		step := time.Duration(attempt*attempt) * 25 * time.Millisecond
		seen := map[time.Duration]bool{}
		for i := 0; i < 200; i++ {
			w := abortWait(attempt)
			if w < step/2 || w > step {
				t.Fatalf("try %d waited %v, want %v to %v", attempt, w, step/2, step)
			}
			seen[w] = true
		}
		if len(seen) < 2 {
			t.Errorf("try %d always waited the same: no jitter", attempt)
		}
		most += step
	}
	if most > 3500*time.Millisecond {
		t.Errorf("the tries wait up to %v together", most)
	}
}

// A write Dgraph aborts for a concurrent one is run again; one still aborted
// after the last try is a conflict the person can act on (409, "Someone
// changed this at the same moment"), not a 500. On 6 Oct a board move failed
// this way, answering 500 "Failed to move the task".
func TestAWriteAbortedEveryTimeIsAConflict(t *testing.T) {
	tries := 0
	err := retryAborted(context.Background(), func() error { tries++; return dgo.ErrAborted })
	if tries != abortRetries {
		t.Errorf("tried %d times, want %d", tries, abortRetries)
	}
	if !errors.Is(err, helpers.ErrConflict) {
		t.Errorf("err %v isn't a conflict", err)
	}
	if !errors.Is(err, dgo.ErrAborted) {
		t.Errorf("err %v hides the abort", err)
	}
}

func TestAWriteAbortedOnceLandsOnTheNextTry(t *testing.T) {
	tries := 0
	err := retryAborted(context.Background(), func() error {
		tries++
		if tries == 1 {
			return dgo.ErrAborted
		}
		return nil
	})
	if err != nil || tries != 2 {
		t.Errorf("got %v after %d tries, want success on the second", err, tries)
	}
}

func TestAnyOtherFailureIsReturnedAsItIs(t *testing.T) {
	tries := 0
	unavailable := errors.New("rpc error: code = Unavailable")
	err := retryAborted(context.Background(), func() error { tries++; return unavailable })
	if err != unavailable || tries != 1 {
		t.Errorf("got %v after %d tries, want the failure itself after one", err, tries)
	}
	if errors.Is(err, helpers.ErrConflict) {
		t.Error("a store that didn't answer was called a conflict")
	}
}
