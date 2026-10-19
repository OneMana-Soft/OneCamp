package business

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// No rule exists yet, so no task is on a timer.
func TestNoTaskIsOnATimerYet(t *testing.T) {
	if s, due, err := SLAs.StateOf(context.Background(), uuid.New()); s != None || due != nil || err != nil {
		t.Errorf("%q %v %v", s, due, err)
	}
}
