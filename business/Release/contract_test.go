package business

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
	"github.com/google/uuid"
)

// Releases are built (P10-1b): the book reads releases, and one that isn't
// there is not found, never "not built".
func TestReleasesAreBuilt(t *testing.T) {
	was := getRow
	t.Cleanup(func() { getRow = was })
	getRow = func(context.Context, uuid.UUID) (releaseModel.Release, bool, error) {
		return releaseModel.Release{}, false, nil
	}
	if _, ok := Releases.(book); !ok {
		t.Fatalf("Releases is %T, want the built book", Releases)
	}
	if _, err := Releases.Get(context.Background(), accessBusiness.Subject{}, uuid.New()); !errors.Is(err, helpers.ErrNotFound) {
		t.Errorf("a release that isn't there: %v, want not found", err)
	}
}
