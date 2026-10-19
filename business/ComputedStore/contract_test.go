package business

import (
	"context"
	"errors"
	"testing"

	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
)

// Without business/DataTable nothing is worked out.
func TestNoStoreWorksNothingOut(t *testing.T) {
	if err := Values.Read(context.Background(), tableAccess.Who{}, nil, nil); !errors.Is(err, ErrNotRegistered) {
		t.Errorf("read: %v", err)
	}
}
