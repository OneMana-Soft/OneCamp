package business

import (
	"context"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	computedStore "github.com/akashc777/OneCamp/business/ComputedStore"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// The contracts answer with this package's code once it's in the binary:
// the row ledger's writer and log (A2), its keyed windows, today's computed
// cells and access rule.
func TestTheContractsAreThisPackages(t *testing.T) {
	if _, ok := rowLedger.Rows.(ledgerWriter); !ok {
		t.Errorf("the row ledger's writer is %T", rowLedger.Rows)
	}
	if _, ok := rowLedger.Log.(ledgerLog); !ok {
		t.Errorf("the row ledger's log is %T", rowLedger.Log)
	}
	if _, ok := rowQuery.Rows.(keyedRows); !ok {
		t.Errorf("row query's engine is %T", rowQuery.Rows)
	}
	if _, ok := computedStore.Values.(computedValues); !ok {
		t.Errorf("the computed store is %T", computedStore.Values)
	}
	if _, ok := tableAccess.Tables.(todaysRule); !ok {
		t.Errorf("table access is %T", tableAccess.Tables)
	}
}

// A window's cursor is the last row's place and id: one that isn't is
// refused before anything is read, and the first window has none.
func TestAWindowsCursorIsAPlaceAndARow(t *testing.T) {
	ctx, table := context.Background(), uuid.New()
	if _, err := rowQuery.Rows.Window(ctx, tableAccess.Who{}, table, "first", 10); err == nil {
		t.Error("a cursor that isn't one was read")
	}
	id := uuid.New()
	key, after, err := readCursor(rowQuery.Cursor("d0001|" + id.String()))
	if err != nil || key != "d0001" || after != id {
		t.Errorf("a cursor reads as %q %v (%v)", key, after, err)
	}
	if key, after, err := readCursor(""); err != nil || key != "" || after != uuid.Nil {
		t.Errorf("no cursor reads as %q %v (%v), want the first window", key, after, err)
	}
	// A row with no place yet sorts last: its cursor has no key.
	if key, after, err := readCursor(rowQuery.Cursor("|" + id.String())); err != nil || key != "" || after != id {
		t.Errorf("a cursor at a row with no place reads as %q %v (%v)", key, after, err)
	}
}

// The Access Resolver's "table" is today's rule: its creator and admins have
// it whole, anyone in the workspace writes rows of a workspace table, and a
// deleted one is none for everyone.
func TestTableDecisions(t *testing.T) {
	owner, member, admin := uuid.New(), uuid.New(), uuid.New()
	shared := &model.DataTable{Id: uuid.New(), CreatedBy: owner, Visibility: model.VisibilityWorkspace}
	private := &model.DataTable{Id: uuid.New(), CreatedBy: owner, Visibility: model.VisibilityPrivate}
	gone := time.Now()
	deleted := &model.DataTable{Id: uuid.New(), CreatedBy: owner, Visibility: model.VisibilityWorkspace, DeletedAt: &gone}
	cases := []struct {
		name  string
		t     *model.DataTable
		a     Actor
		level accessBusiness.Level
		owner bool
	}{
		{"its creator", private, Actor{UserID: owner}, accessBusiness.LevelFull, true},
		{"an admin", private, Actor{UserID: admin, IsAdmin: true}, accessBusiness.LevelFull, false},
		{"a member, a workspace table", shared, Actor{UserID: member}, accessBusiness.LevelEdit, false},
		{"a member, a private table", private, Actor{UserID: member}, accessBusiness.LevelNone, false},
		{"nobody", shared, Actor{}, accessBusiness.LevelEdit, false},
	}
	for _, c := range cases {
		d := tableDecision(c.t, c.a)
		if d.Level != c.level || d.Owner != c.owner || d.Was != c.level || d.Deleted {
			t.Errorf("%s: %+v", c.name, d)
		}
		if (d.Level == accessBusiness.LevelNone) != (len(d.Reasons) == 0) {
			t.Errorf("%s: reasons %v for %v", c.name, d.Reasons, d.Level)
		}
		// The rule tables' own code applies agrees.
		if view := canView(c.t, c.a); view != (d.Level >= accessBusiness.LevelEdit) {
			t.Errorf("%s: canView %v, the resolver %v", c.name, view, d.Level)
		}
		if manage := canManage(c.t, c.a); manage != (d.Level == accessBusiness.LevelFull) {
			t.Errorf("%s: canManage %v, the resolver %v", c.name, manage, d.Level)
		}
	}
	d := tableDecision(deleted, Actor{UserID: owner})
	if d.Level != accessBusiness.LevelNone || !d.Deleted || d.Was != accessBusiness.LevelFull || !d.Owner {
		t.Errorf("a deleted table, for its creator: %+v", d)
	}
}
