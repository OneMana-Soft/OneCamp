package business

import (
	"context"
	"errors"
	"testing"

	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	"github.com/google/uuid"
)

// Without business/DataTable nothing is written, and nothing is recorded to
// read or undo yet.
func TestTheLedgerAtDayZero(t *testing.T) {
	ctx, by, table := context.Background(), By{Kind: ActorPerson}, uuid.New()
	if _, err := Rows.Create(ctx, by, table, []RowInput{{}}, Position{}); !errors.Is(err, ErrNotRegistered) {
		t.Errorf("create: %v", err)
	}
	if _, err := Rows.Patch(ctx, by, table, []RowPatch{{Row: uuid.New()}}); !errors.Is(err, ErrNotRegistered) {
		t.Errorf("patch: %v", err)
	}
	if err := Rows.Delete(ctx, by, table, []uuid.UUID{uuid.New()}); !errors.Is(err, ErrNotRegistered) {
		t.Errorf("delete: %v", err)
	}
	who := tableAccess.Who{User: uuid.New()}
	if _, err := Log.History(ctx, who, table, uuid.New(), Page{}); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("history: %v", err)
	}
	if _, err := Log.Undo(ctx, by, table, nil); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("undo: %v", err)
	}
	if _, err := Log.Redo(ctx, by, table); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("redo: %v", err)
	}
	if _, err := Log.Trash(ctx, who, table, Page{}); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("trash: %v", err)
	}
	if _, err := Log.Restore(ctx, by, table, nil); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("restore: %v", err)
	}
}

// Observers are heard in the order they registered; one with no name, or a
// name taken, stops the build's tests.
func TestObservers(t *testing.T) {
	r := &ObserverRegistry{}
	r.Register(Observer{Name: "cell index"})
	r.Register(Observer{Name: "live rows"})
	if got := r.All(); len(got) != 2 || got[0].Name != "cell index" || got[1].Name != "live rows" {
		t.Fatalf("observers %v", got)
	}
	for _, o := range []Observer{{Name: "cell index"}, {}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("registering %+v didn't panic", o)
				}
			}()
			r.Register(o)
		}()
	}
}
