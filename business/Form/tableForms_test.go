package business

import (
	"context"
	"errors"
	"testing"

	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	"github.com/google/uuid"
)

// asTableManager stands in for the store and Table Access: decision is what
// anyone may do with table, and the store's calls are counted.
func asTableManager(t *testing.T, table uuid.UUID, decision tableAccess.Decision, existing int) (stored *int, made *formModel.Form) {
	t.Helper()
	saved := struct {
		who    func(context.Context, uuid.UUID) (tableAccess.Who, error)
		access func(context.Context, tableAccess.Who, uuid.UUID) (tableAccess.Decision, error)
		list   func(context.Context, uuid.UUID) ([]formModel.Form, error)
		create func(context.Context, formModel.Form, int) (*formModel.Form, int, error)
		update func(context.Context, uuid.UUID, formModel.Form) (*formModel.Form, error)
		del    func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error)
		get    func(context.Context, uuid.UUID, uuid.UUID) (*formModel.Form, error)
	}{whoIs, tableDecision, tableForms, createForm, updateTableForm, deleteTableForm, tableFormOf}
	t.Cleanup(func() {
		whoIs, tableDecision, tableForms = saved.who, saved.access, saved.list
		createForm, updateTableForm, deleteTableForm, tableFormOf = saved.create, saved.update, saved.del, saved.get
	})
	calls := 0
	made = &formModel.Form{}
	whoIs = func(_ context.Context, id uuid.UUID) (tableAccess.Who, error) { return tableAccess.Who{User: id}, nil }
	tableDecision = func(_ context.Context, _ tableAccess.Who, id uuid.UUID) (tableAccess.Decision, error) {
		if id != table {
			return tableAccess.Decision{}, tableAccess.ErrNotFound
		}
		return decision, nil
	}
	tableForms = func(context.Context, uuid.UUID) ([]formModel.Form, error) { calls++; return []formModel.Form{}, nil }
	// The store counts and makes in one step: with limit there already, it
	// makes nothing.
	createForm = func(_ context.Context, f formModel.Form, limit int) (*formModel.Form, int, error) {
		calls++
		if existing >= limit {
			return nil, existing, nil
		}
		*made = f
		return &f, existing, nil
	}
	updateTableForm = func(_ context.Context, _ uuid.UUID, f formModel.Form) (*formModel.Form, error) {
		calls++
		*made = f
		return &f, nil
	}
	deleteTableForm = func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) { calls++; return true, nil }
	tableFormOf = func(context.Context, uuid.UUID, uuid.UUID) (*formModel.Form, error) {
		calls++
		return &formModel.Form{}, nil
	}
	return &calls, made
}

// A table's own forms are its set-up: someone who can read the table and add
// its rows, but not manage it, can't list, make, change, read or delete
// them, and the store is never asked; a table they can't see, or that isn't
// there, is not found.
func TestATablesFormsAreItsManagersAlone(t *testing.T) {
	table, cols := vendors()
	by := uuid.New()
	in := Input{Title: "Vendor sign-up", Fields: signUp(cols)}
	every := func(table uuid.UUID) map[string]error {
		_, list := TableForms(context.Background(), table, by)
		_, save := SaveTableForm(context.Background(), table, in, by)
		_, del := DeleteTableForm(context.Background(), table, uuid.New(), by)
		_, get := TableForm(context.Background(), table, uuid.New(), by)
		return map[string]error{"list": list, "save": save, "delete": del, "read": get}
	}
	for name, d := range map[string]tableAccess.Decision{"an editor": {View: true, Edit: true}, "a reader": {View: true}} {
		calls, _ := asTableManager(t, table, d, 0)
		for what, err := range every(table) {
			if !errors.Is(err, ErrTableForbidden) {
				t.Errorf("%s's %s: %v, want ErrTableForbidden", name, what, err)
			}
		}
		if *calls != 0 {
			t.Errorf("%s reached the store %d times", name, *calls)
		}
	}
	calls, _ := asTableManager(t, table, tableAccess.Decision{}, 0)
	for what, err := range every(table) {
		if !errors.Is(err, ErrTableNotFound) {
			t.Errorf("a table they can't see, %s: %v, want ErrTableNotFound", what, err)
		}
	}
	for what, err := range every(uuid.New()) {
		if !errors.Is(err, ErrTableNotFound) {
			t.Errorf("a table that isn't there, %s: %v, want ErrTableNotFound", what, err)
		}
	}
	if *calls != 0 {
		t.Errorf("refusals reached the store %d times", *calls)
	}
}

// A table's manager makes its forms: each adds rows to that table whatever
// the request names, has no project, task priority or assignee, and is owned
// by whoever saved it; a table keeps at most 20.
func TestATablesOwnFormAddsRowsToItsTable(t *testing.T) {
	table, cols := vendors()
	withTable(t, table, cols, true)
	manage := tableAccess.Decision{View: true, Edit: true, Manage: true}
	calls, made := asTableManager(t, table, manage, 0)
	by := uuid.New()
	in := Input{Title: "Vendor sign-up", Fields: signUp(cols), TargetTable: uuid.NewString(), Priority: "high", AssigneeUUID: uuid.NewString(), Active: true}
	f, err := SaveTableForm(context.Background(), table, in, by)
	if err != nil {
		t.Fatal(err)
	}
	if f.TargetTable == nil || *made.TargetTable != table || made.ProjectUUID != uuid.Nil || made.AssigneeUUID != nil || made.CreatedBy != by || len(made.Token) != 24 {
		t.Errorf("made %+v: want table %s, no project or assignee, owned by %s, with a link", made, table, by)
	}
	if *calls != 1 {
		t.Errorf("store calls %d, want one create, counted in the same step", *calls)
	}
	// A change keeps its id; its new owner is whoever saved it.
	in.Id = uuid.NewString()
	other := uuid.New()
	if _, err := SaveTableForm(context.Background(), table, in, other); err != nil || made.Id.String() != in.Id || made.CreatedBy != other {
		t.Errorf("changed %+v (%v): want id %s owned by %s", made, err, in.Id, other)
	}
	// The 21st is refused, in words.
	asTableManager(t, table, manage, maxForms)
	in.Id = ""
	var fe *FormError
	if _, err := SaveTableForm(context.Background(), table, in, by); !errors.As(err, &fe) || fe.Error() != "This table has 20 forms. Delete one to make another." {
		t.Errorf("a 21st form: %v, want the table's FormError", err)
	}
}

// A table's own form takes answers only while its owner can still manage the
// table: one made an editor, or who lost the table, closes it.
func TestATablesOwnFormClosesWhenItsOwnerCantManageIt(t *testing.T) {
	table, _ := vendors()
	l, _ := visitorForm(t)
	l.form.TargetTable = &table
	asTableManager(t, table, tableAccess.Decision{View: true, Edit: true, Manage: true}, 0)
	if !tableFormOpen(context.Background(), l) {
		t.Error("closed while its owner manages the table")
	}
	asTableManager(t, table, tableAccess.Decision{View: true, Edit: true}, 0)
	if tableFormOpen(context.Background(), l) {
		t.Error("open after its owner became an editor")
	}
	other := uuid.New()
	l.form.TargetTable = &other
	if tableFormOpen(context.Background(), l) {
		t.Error("open on a table that isn't there")
	}
}
