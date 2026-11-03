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
	table := func(visibility, general string) *model.DataTable {
		return &model.DataTable{Id: uuid.New(), CreatedBy: owner, Visibility: visibility, GeneralLevel: general}
	}
	shared := table(model.VisibilityWorkspace, model.GeneralEdit)
	private := table(model.VisibilityPrivate, model.GeneralEdit)
	commenting := table(model.VisibilityWorkspace, model.GeneralComment)
	viewing := table(model.VisibilityWorkspace, model.GeneralView)
	unfilled := table(model.VisibilityWorkspace, "")
	privateComment := table(model.VisibilityPrivate, model.GeneralComment)
	gone := time.Now()
	deleted := &model.DataTable{Id: uuid.New(), CreatedBy: owner, Visibility: model.VisibilityWorkspace, GeneralLevel: model.GeneralEdit, DeletedAt: &gone}
	cases := []struct {
		name   string
		t      *model.DataTable
		a      Actor
		chosen bool
		level  accessBusiness.Level
		owner  bool
		reason string
	}{
		{"its creator", private, Actor{UserID: owner}, false, accessBusiness.LevelFull, true, "owner"},
		{"an admin", private, Actor{UserID: admin, IsAdmin: true}, false, accessBusiness.LevelFull, false, reasonAdmin},
		{"a member, a workspace table", shared, Actor{UserID: member}, false, accessBusiness.LevelEdit, false, "workspace"},
		{"a member, a private table", private, Actor{UserID: member}, false, accessBusiness.LevelNone, false, ""},
		{"nobody", shared, Actor{}, false, accessBusiness.LevelEdit, false, "workspace"},
		// V-170: who edits a workspace table's rows.
		{"a member, a table only chosen people edit", commenting, Actor{UserID: member}, false, accessBusiness.LevelComment, false, "workspace"},
		{"a chosen member, a table only chosen people edit", commenting, Actor{UserID: member}, true, accessBusiness.LevelEdit, false, reasonEditor},
		{"a chosen member, a table everyone edits", shared, Actor{UserID: member}, true, accessBusiness.LevelEdit, false, "workspace"},
		{"its creator, a table only chosen people edit", commenting, Actor{UserID: owner}, false, accessBusiness.LevelFull, true, "owner"},
		{"an admin, a table only chosen people edit", commenting, Actor{UserID: admin, IsAdmin: true}, false, accessBusiness.LevelFull, false, reasonAdmin},
		{"nobody, a table only chosen people edit", commenting, Actor{}, false, accessBusiness.LevelComment, false, "workspace"},
		{"a member, a table its members view", viewing, Actor{UserID: member}, false, accessBusiness.LevelView, false, "workspace"},
		{"a member, a level the database didn't fill", unfilled, Actor{UserID: member}, false, accessBusiness.LevelView, false, "workspace"},
		{"a chosen member, a private table", privateComment, Actor{UserID: member}, true, accessBusiness.LevelNone, false, ""},
	}
	for _, c := range cases {
		d := tableDecision(c.t, c.a, c.chosen)
		if d.Level != c.level || d.Owner != c.owner || d.Was != c.level || d.Deleted {
			t.Errorf("%s: %+v", c.name, d)
		}
		if (d.Level == accessBusiness.LevelNone) != (len(d.Reasons) == 0) {
			t.Errorf("%s: reasons %v for %v", c.name, d.Reasons, d.Level)
		}
		if c.reason != "" && (len(d.Reasons) != 1 || d.Reasons[0] != c.reason) {
			t.Errorf("%s: reasons %v, want [%s]", c.name, d.Reasons, c.reason)
		}
		// The rule tables' own code applies agrees.
		if view := canView(c.t, c.a); view != (d.Level >= accessBusiness.LevelView) {
			t.Errorf("%s: canView %v, the resolver %v", c.name, view, d.Level)
		}
		if edit := rowsEditable(c.t, c.a, c.chosen); edit != (d.Level >= accessBusiness.LevelEdit) {
			t.Errorf("%s: rowsEditable %v, the resolver %v", c.name, edit, d.Level)
		}
		if manage := canManage(c.t, c.a); manage != (d.Level == accessBusiness.LevelFull) {
			t.Errorf("%s: canManage %v, the resolver %v", c.name, manage, d.Level)
		}
	}
	d := tableDecision(deleted, Actor{UserID: owner}, false)
	if d.Level != accessBusiness.LevelNone || !d.Deleted || d.Was != accessBusiness.LevelFull || !d.Owner {
		t.Errorf("a deleted table, for its creator: %+v", d)
	}
}
