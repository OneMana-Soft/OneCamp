package business

import (
	"context"
	"errors"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// rowWorld is two tables and their rows, read through the seams.
type rowAccessWorld struct {
	homes  map[uuid.UUID]model.RowHome
	tables map[uuid.UUID]*model.DataTable
	fields map[uuid.UUID][]*model.Field
	cells  map[uuid.UUID]string
}

func (w *rowAccessWorld) install(t *testing.T) {
	t.Helper()
	oldHomes, oldTables, oldFields, oldCells := rowHomes, tablesByIDs, fieldsOf, cellsOf
	t.Cleanup(func() { rowHomes, tablesByIDs, fieldsOf, cellsOf = oldHomes, oldTables, oldFields, oldCells })
	rowHomes = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]model.RowHome, error) {
		out := map[uuid.UUID]model.RowHome{}
		for _, id := range ids {
			if h, ok := w.homes[id]; ok {
				out[id] = h
			}
		}
		return out, nil
	}
	tablesByIDs = func(_ context.Context, ids []uuid.UUID) ([]*model.DataTable, error) {
		var out []*model.DataTable
		for _, id := range ids {
			if tb, ok := w.tables[id]; ok {
				out = append(out, tb)
			}
		}
		return out, nil
	}
	fieldsOf = func(_ context.Context, id uuid.UUID) ([]*model.Field, error) { return w.fields[id], nil }
	cellsOf = func(_ context.Context, table uuid.UUID, ids []uuid.UUID, _ []string) ([]*model.Row, error) {
		var out []*model.Row
		for _, id := range ids {
			if v, ok := w.cells[id]; ok && !w.homes[id].Deleted {
				out = append(out, &model.Row{Id: id, TableId: table, Values: v})
			}
		}
		return out, nil
	}
}

// A row is decided by its own table: a private table's rows are its
// creator's and an admin's, a workspace table's everyone's; a row in the
// trash is nobody's outside it, with what they had kept; an id that names no
// row, or not as a uuid, is left out.
func TestARowIsDecidedByItsOwnTable(t *testing.T) {
	maker, member := uuid.New(), uuid.New()
	private, shared := uuid.New(), uuid.New()
	secret, open, binned, orphan := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	w := &rowAccessWorld{
		homes: map[uuid.UUID]model.RowHome{
			secret: {Table: private}, open: {Table: shared}, binned: {Table: shared, Deleted: true}, orphan: {Table: uuid.New()},
		},
		tables: map[uuid.UUID]*model.DataTable{
			private: {Id: private, CreatedBy: maker, Visibility: model.VisibilityPrivate},
			shared:  {Id: shared, CreatedBy: maker, Visibility: model.VisibilityWorkspace},
		},
	}
	w.install(t)
	ids := []string{secret.String(), open.String(), binned.String(), orphan.String(), "not-a-row"}
	decide := func(who accessBusiness.Subject) map[string]accessBusiness.Decision {
		t.Helper()
		got, err := rowKind{}.DecideMany(context.Background(), who, ids)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	asMember := decide(accessBusiness.Subject{UserUUID: member.String()})
	if d := asMember[secret.String()]; d.Level != accessBusiness.LevelNone {
		t.Errorf("a member on a private table's row: %v", d.Level)
	}
	if d := asMember[open.String()]; d.Level != accessBusiness.LevelEdit {
		t.Errorf("a member on a workspace table's row: %v", d.Level)
	}
	if d := asMember[binned.String()]; !d.Deleted || d.Level != accessBusiness.LevelNone || d.Was != accessBusiness.LevelEdit {
		t.Errorf("a row in the trash: %+v", d)
	}
	if _, ok := asMember[orphan.String()]; ok {
		t.Error("a row whose table isn't there was decided")
	}
	if _, ok := asMember["not-a-row"]; ok {
		t.Error("an id that isn't a uuid was decided")
	}
	if d := decide(accessBusiness.Subject{UserUUID: maker.String()})[secret.String()]; d.Level != accessBusiness.LevelFull || !d.Owner {
		t.Errorf("the maker on their private table's row: %+v", d)
	}
	if d := decide(accessBusiness.Subject{UserUUID: member.String(), Admin: true})[secret.String()]; d.Level != accessBusiness.LevelFull {
		t.Errorf("an admin on a private table's row: %v", d.Level)
	}
	// A table in the trash decides its rows as it decides itself.
	gone := time.Now()
	w.tables[shared].DeletedAt = &gone
	if d := decide(accessBusiness.Subject{UserUUID: member.String()})[open.String()]; !d.Deleted || d.Level != accessBusiness.LevelNone {
		t.Errorf("a row of a table in the trash: %+v", d)
	}
}

// Rows are named as their links name them, with their tables' names; a row in
// the trash is "A deleted row", and a table with no name "Untitled table".
func TestRowsAreNamedAsTheirLinksNameThem(t *testing.T) {
	deals, notes := uuid.New(), uuid.New()
	name := &model.Field{Id: uuid.New(), TableId: deals, Name: "Name", Type: model.FieldText, Primary: true}
	acme, blank, binned, memo := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	w := &rowAccessWorld{
		homes: map[uuid.UUID]model.RowHome{acme: {Table: deals}, blank: {Table: deals}, binned: {Table: deals, Deleted: true}, memo: {Table: notes}},
		tables: map[uuid.UUID]*model.DataTable{
			deals: {Id: deals, Name: " Deals "},
			notes: {Id: notes, Name: ""},
		},
		fields: map[uuid.UUID][]*model.Field{deals: {name}},
		cells: map[uuid.UUID]string{
			acme: `{"` + name.Id.String() + `":"Acme renewal"}`, blank: `{}`, binned: `{"` + name.Id.String() + `":"Gone"}`, memo: `{}`,
		},
	}
	w.install(t)
	got, err := RowTitles(context.Background(), []uuid.UUID{acme, blank, binned, memo, uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if r := got[acme]; r.Title != "Acme renewal" || r.TableName != "Deals" || r.Table != deals || r.Deleted {
		t.Errorf("a named row: %+v", r)
	}
	if r := got[blank]; r.Title != "Untitled" {
		t.Errorf("a row with no name: %+v", r)
	}
	if r := got[binned]; r.Title != deletedRow || !r.Deleted {
		t.Errorf("a row in the trash: %+v", r)
	}
	if r := got[memo]; r.TableName != "Untitled table" || r.Title != "Untitled" {
		t.Errorf("a row of a table with no name or fields: %+v", r)
	}
	if len(got) != 4 {
		t.Errorf("%d rows named; a row that isn't there is left out", len(got))
	}
}

// A member's rows carry how many comments their open threads hold; counts that
// can't be read leave the rows as they are, never failing the read.
func TestRowsCarryTheirCommentCounts(t *testing.T) {
	old := rowCommentCounts
	t.Cleanup(func() { rowCommentCounts = old })
	a, b := uuid.New(), uuid.New()
	rows := []*model.Row{{Id: a}, {Id: b}}
	rowCommentCounts = func(_ context.Context, ids []string) (map[string]int, error) {
		if len(ids) != 2 {
			t.Errorf("counted %d rows", len(ids))
		}
		return map[string]int{a.String(): 3}, nil
	}
	WithCommentCounts(context.Background(), rows)
	if rows[0].Comments != 3 || rows[1].Comments != 0 {
		t.Errorf("counts %d and %d", rows[0].Comments, rows[1].Comments)
	}
	rowCommentCounts = func(context.Context, []string) (map[string]int, error) { return nil, errors.New("down") }
	fresh := []*model.Row{{Id: a}}
	WithCommentCounts(context.Background(), fresh)
	if fresh[0].Comments != 0 {
		t.Error("a count that couldn't be read was guessed")
	}
	WithCommentCounts(context.Background(), nil) // nothing to count asks nothing
}
