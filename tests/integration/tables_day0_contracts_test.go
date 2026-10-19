//go:build integration
// +build integration

package integration_test

// The tables programme's day-0 contracts against Postgres 12 with every
// migration: each answers as the code it names does today
// (design/tables-vs-airtable/PROGRAM.md §5; business/DataTable day0.go).
//
// Run: go test -tags=integration ./tests/integration/ -run TestTablesDay0Contracts -v

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	computedStore "github.com/akashc777/OneCamp/business/ComputedStore"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func TestTablesDay0Contracts(t *testing.T) {
	w := newTableWorld(t, "amara", "bo", "cy")
	who := func(name string) tableAccess.Who { return tableAccess.Who{User: w.people[name].UserID} }
	by := func(name string) rowLedger.By { return rowLedger.By{Who: who(name), Kind: rowLedger.ActorPerson} }
	deals := w.table("amara", "Deals")
	notes := w.field("amara", deals, "Notes", dataTableModel.FieldText, nil)
	amount := w.field("amara", deals, "Amount", dataTableModel.FieldNumber, nil)

	t.Run("the writer writes as CreateRow, PatchRow and DeleteRow do", func(t *testing.T) {
		made, err := rowLedger.Rows.Create(w.ctx, by("bo"), deals.Id, []rowLedger.RowInput{
			{Cells: map[string]any{notes: "first", amount: 1.0}},
			{Cells: map[string]any{notes: "second", amount: 2.0}},
		}, rowLedger.Position{})
		if err != nil || len(made) != 2 {
			t.Fatalf("create: %v %v", made, err)
		}
		if made[1].Position <= made[0].Position {
			t.Errorf("the second row went at %v, before the first at %v", made[1].Position, made[0].Position)
		}
		if _, err := rowLedger.Rows.Patch(w.ctx, by("bo"), deals.Id, []rowLedger.RowPatch{{Row: made[0].Id, Cells: map[string]any{notes: "edited"}}}); err != nil {
			t.Fatalf("patch: %v", err)
		}
		if v := w.stored(deals, made[0].Id); v[notes] != "edited" || v[amount] != 1.0 {
			t.Errorf("after the patch the row holds %v", v)
		}
		if err := rowLedger.Rows.Delete(w.ctx, by("bo"), deals.Id, []uuid.UUID{made[1].Id}); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if r, _ := dataTableModel.GetRowByID(w.ctx, deals.Id, made[1].Id); r != nil {
			t.Error("the deleted row is still there")
		}
		if err := rowLedger.Rows.Delete(w.ctx, by("bo"), deals.Id, []uuid.UUID{made[1].Id}); !dataTableBusiness.IsRowNotFound(err) {
			t.Errorf("deleting it again: %v", err)
		}
	})

	t.Run("windows and counts are today's pages", func(t *testing.T) {
		tb := w.table("amara", "Pages")
		name := w.field("amara", tb, "Item", dataTableModel.FieldText, nil)
		var want []uuid.UUID
		for i := 0; i < 5; i++ {
			r, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]any{name: string(rune('a' + i))}}, w.people["amara"])
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, r.Id)
		}
		var got []uuid.UUID
		from := rowQuery.Cursor("")
		for pages := 0; pages < 10; pages++ {
			win, err := rowQuery.Rows.Window(w.ctx, who("bo"), tb.Id, from, 2)
			if err != nil {
				t.Fatalf("window from %q: %v", from, err)
			}
			for _, r := range win.Rows {
				got = append(got, r.Id)
			}
			if win.Next == "" {
				break
			}
			from = win.Next
		}
		if len(got) != len(want) {
			t.Fatalf("the windows held %d rows, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("row %d is %v, want %v (the table's order)", i, got[i], want[i])
			}
		}
		if n, err := rowQuery.Rows.Count(w.ctx, who("bo"), tb.Id); err != nil || n != 5 {
			t.Errorf("count %d, %v", n, err)
		}
	})

	t.Run("a private table is read and decided by today's rule", func(t *testing.T) {
		secret, err := dataTableBusiness.CreateTable(w.ctx, dataTableBusiness.TableInput{Name: "Secret", Visibility: "private"}, w.people["amara"])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rowQuery.Rows.Window(w.ctx, who("bo"), secret.Id, "", 10); !dataTableBusiness.IsForbidden(err) {
			t.Errorf("Bo read a private table's rows: %v", err)
		}
		if _, err := rowQuery.Rows.Count(w.ctx, who("bo"), secret.Id); !dataTableBusiness.IsForbidden(err) {
			t.Errorf("Bo counted a private table's rows: %v", err)
		}
		admin := tableAccess.Who{User: w.people["cy"].UserID, Admin: true}
		cases := []struct {
			name  string
			who   tableAccess.Who
			table *dataTableModel.DataTable
			want  tableAccess.Decision
		}{
			{"its creator", who("amara"), secret, tableAccess.Decision{View: true, Edit: true, Manage: true}},
			{"a member", who("bo"), secret, tableAccess.Decision{}},
			{"an admin", admin, secret, tableAccess.Decision{View: true, Edit: true, Manage: true}},
			{"a member, a workspace table", who("bo"), deals, tableAccess.Decision{View: true, Edit: true}},
		}
		for _, c := range cases {
			d, err := tableAccess.Tables.Resolve(w.ctx, c.who, c.table.Id)
			if err != nil || d != c.want {
				t.Errorf("%s: %+v %v, want %+v", c.name, d, err, c.want)
			}
		}
		if _, err := tableAccess.Tables.Resolve(w.ctx, who("amara"), uuid.New()); !errors.Is(err, tableAccess.ErrNotFound) {
			t.Errorf("a table that isn't there: %v", err)
		}
	})

	t.Run("the Access Resolver decides tables, a deleted one too", func(t *testing.T) {
		gone := w.table("amara", "Gone")
		if err := dataTableBusiness.DeleteTable(w.ctx, gone.Id, w.people["amara"]); err != nil {
			t.Fatal(err)
		}
		subject := func(name string) accessBusiness.Subject {
			return accessBusiness.Subject{UserUUID: w.people[name].UserID.String()}
		}
		got, err := accessBusiness.ResolveAll(w.ctx, subject("bo"), []accessBusiness.Resource{
			{Kind: tableAccess.KindTable, UUID: deals.Id.String()},
			{Kind: tableAccess.KindTable, UUID: gone.Id.String()},
			{Kind: tableAccess.KindTable, UUID: uuid.NewString()},
		})
		if err != nil {
			t.Fatal(err)
		}
		if d := got[accessBusiness.Resource{Kind: tableAccess.KindTable, UUID: deals.Id.String()}]; d.Level != accessBusiness.LevelEdit {
			t.Errorf("Bo on a workspace table: %+v", d)
		}
		if d := got[accessBusiness.Resource{Kind: tableAccess.KindTable, UUID: gone.Id.String()}]; d.Level != accessBusiness.LevelNone || !d.Deleted || d.Was != accessBusiness.LevelEdit {
			t.Errorf("Bo on a deleted table: %+v", d)
		}
		if len(got) != 2 {
			t.Errorf("a table that isn't there was decided: %v", got)
		}
		d, err := accessBusiness.Rules.Resolve(w.ctx, subject("amara"), accessBusiness.Resource{Kind: tableAccess.KindTable, UUID: deals.Id.String()})
		if err != nil || d.Level != accessBusiness.LevelFull || !d.Owner {
			t.Errorf("Amara on her table: %+v %v", d, err)
		}
	})

	t.Run("computed cells are worked out on read", func(t *testing.T) {
		double := w.field("amara", deals, "Double", dataTableModel.FieldFormula, map[string]any{"formula": "{Amount} * 2"})
		r, err := dataTableBusiness.CreateRow(w.ctx, deals.Id, dataTableBusiness.RowInput{Values: map[string]any{amount: 21.0}}, w.people["amara"])
		if err != nil {
			t.Fatal(err)
		}
		stored, err := dataTableModel.GetRowByID(w.ctx, deals.Id, r.Id)
		if err != nil || stored == nil {
			t.Fatalf("read the row: %v", err)
		}
		fields, err := dataTableModel.ListFields(w.ctx, deals.Id)
		if err != nil {
			t.Fatal(err)
		}
		if err := computedStore.Values.Read(w.ctx, who("bo"), fields, []*dataTableModel.Row{stored}); err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(stored.Values), &v); err != nil {
			t.Fatal(err)
		}
		if v[double] != 42.0 {
			t.Errorf("the formula gave %v, want 42", v[double])
		}
	})
}
