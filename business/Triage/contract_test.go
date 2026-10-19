package business

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// No project has triage yet: nothing waits, and nothing is decided.
func TestTriageIsntBuiltYet(t *testing.T) {
	if got, err := Triage.Waiting(context.Background(), nil, uuid.New()); err != nil || len(got) != 0 {
		t.Errorf("%v %v", got, err)
	}
	if err := Triage.Decide(context.Background(), nil, uuid.New(), Accept, ""); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("%v", err)
	}
}
