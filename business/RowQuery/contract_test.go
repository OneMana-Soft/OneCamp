//go:build !integration

package business

// Only a build without the integration tests: they link business/DataTable,
// whose init registers its engine, so Rows has one there.

import (
	"context"
	"errors"
	"testing"

	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	"github.com/google/uuid"
)

// Without business/DataTable no rows are read.
func TestNoEngineReadsNothing(t *testing.T) {
	ctx, who, table := context.Background(), tableAccess.Who{User: uuid.New()}, uuid.New()
	if _, err := Rows.Window(ctx, who, table, "", 10); !errors.Is(err, ErrNotRegistered) {
		t.Errorf("window: %v", err)
	}
	if _, err := Rows.Count(ctx, who, table); !errors.Is(err, ErrNotRegistered) {
		t.Errorf("count: %v", err)
	}
}
