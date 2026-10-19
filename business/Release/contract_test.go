package business

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/google/uuid"
)

// There are no releases yet.
func TestThereAreNoReleasesYet(t *testing.T) {
	if got, err := Releases.List(context.Background(), accessBusiness.Subject{}, uuid.New()); err != nil || len(got) != 0 {
		t.Errorf("%v %v", got, err)
	}
	if _, err := Releases.Get(context.Background(), accessBusiness.Subject{}, uuid.New()); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("%v", err)
	}
}
