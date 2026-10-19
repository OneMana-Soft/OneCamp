package business

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Without business/DataTable's rule nothing is allowed.
func TestNoRuleAllowsNothing(t *testing.T) {
	d, err := Tables.Resolve(context.Background(), Who{User: uuid.New(), Admin: true}, uuid.New())
	if !errors.Is(err, ErrNotRegistered) || d != (Decision{}) {
		t.Errorf("decision %+v, %v", d, err)
	}
}
