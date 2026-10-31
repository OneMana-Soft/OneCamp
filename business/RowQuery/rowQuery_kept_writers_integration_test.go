//go:build integration

package business_test

import (
	"context"
	"testing"

	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// Every path that writes a field raises, in its own transaction, the
// generation of each kept value that reads the field (migration 535's
// trigger, the codebase's first: airtable-03, CR-87's M4; since V-108,
// migration 538's, which raises only the readers). Projects.Paid adds up
// Vendors' Amount, Vendors' Label reads its Note and Stage, and Clients'
// Shown reads its Supplier link (the vendors it links to, as Vendors' primary
// field shows them). Each writer below writes a field through its own
// business path, and exactly the kept values that read that field rise, so
// the sweep fills them again and no other: a field made raises none, since
// nothing reads it yet, and a new primary field of Vendors (Code, in place
// of the Name its table was made with) raises what shows Vendors' rows (Paid
// and Shown). A rename, a move or a description changes
// no value, and raises nothing.
func TestEveryFieldWriterRaisesWhatReachesIt(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.SetupDgraph(t)
	db := postgresInit.DBConn.SqlDB
	me := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'kept-writers@example.test', 'kept-writers', 'kept-writers', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}
	table := func(name string) *model.DataTable {
		tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: name, Visibility: "workspace"}, owner)
		if err != nil {
			t.Fatal(err)
		}
		return tb
	}
	add := func(tb *model.DataTable, name, typ string, cfg map[string]interface{}) *model.Field {
		f, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: name, Type: typ, Config: cfg}, owner)
		if err != nil {
			t.Fatalf("field %s: %v", name, err)
		}
		return f
	}
	relation := func(to *model.DataTable, twoWay bool) map[string]interface{} {
		return map[string]interface{}{"relation_target": "table", "table_id": to.Id.String(), "two_way": twoWay}
	}
	vendors := table("Writer vendors")
	amount := add(vendors, "Amount", model.FieldNumber, nil)
	note := add(vendors, "Note", model.FieldText, nil)
	stage := add(vendors, "Stage", model.FieldSelect, map[string]interface{}{"options": []interface{}{map[string]interface{}{"id": "sOpen", "label": "Open"}}})
	projects := table("Writer projects")
	pVendors := add(projects, "Vendors", model.FieldRelation, relation(vendors, false))
	pPaid := add(projects, "Paid", model.FieldRollup, map[string]interface{}{"relation": pVendors.Id.String(), "field": amount.Id.String(), "aggregate": "sum"})
	label := add(vendors, "Label", model.FieldFormula, map[string]interface{}{"formula": "{Note} & {Stage}"})
	clients := table("Writer clients")
	supplier := add(clients, "Supplier", model.FieldRelation, relation(vendors, false))
	shown := add(clients, "Shown", model.FieldFormula, map[string]interface{}{"formula": "{Supplier}"})
	if _, err := dataTable.CreateRow(ctx, vendors.Id, dataTable.RowInput{Values: map[string]interface{}{amount.Id.String(): 100, note.Id.String(): "42"}}, owner); err != nil {
		t.Fatal(err)
	}
	if err := rowQuery.RecomputeKept(ctx); err != nil {
		t.Fatal(err)
	}
	watched := []*model.Field{pPaid, label, shown}
	gens := func() map[uuid.UUID]int64 {
		t.Helper()
		out := map[uuid.UUID]int64{}
		for _, f := range watched {
			var g int64
			if err := db.QueryRowContext(ctx, `SELECT gen FROM data_table_computed WHERE field_id = $1`, f.Id).Scan(&g); err != nil {
				t.Fatal(err)
			}
			out[f.Id] = g
		}
		return out
	}
	for _, f := range []*model.Field{pPaid, shown} {
		var filledReaching int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM data_table_computed WHERE field_id = $1 AND reaches = ARRAY[$2::uuid] AND filled_gen = gen`, f.Id, vendors.Id).Scan(&filledReaching); err != nil || filledReaching != 1 {
			t.Fatalf("%s isn't filled reaching exactly Vendors, so the legs prove nothing: %v", f.Name, err)
		}
	}

	str := func(s string) *string { return &s }
	yes, far := true, 1000.0
	var code *model.Field
	legs := []struct {
		name  string
		write func() error
		rises []*model.Field
	}{
		{"a field made in Vendors (CreateField)", func() (err error) {
			code, err = dataTable.CreateField(ctx, vendors.Id, dataTable.FieldInput{Name: "Code", Type: model.FieldText}, owner)
			return err
		}, nil},
		{"a link pair: Clients links two ways to Vendors, which gets the other side (CreateField)", func() error {
			_, err := dataTable.CreateField(ctx, clients.Id, dataTable.FieldInput{Name: "Vendors", Type: model.FieldRelation, Config: relation(vendors, true)}, owner)
			return err
		}, nil},
		{"Amount's config, no cell rewritten (UpdateField)", func() error {
			_, err := dataTable.UpdateField(ctx, vendors.Id, amount.Id, dataTable.FieldPatch{Config: map[string]interface{}{"format": map[string]interface{}{"decimals": 2}}}, owner)
			return err
		}, []*model.Field{pPaid}},
		{"Code made the primary field (UpdateField)", func() error {
			_, err := dataTable.UpdateField(ctx, vendors.Id, code.Id, dataTable.FieldPatch{Primary: &yes}, owner)
			return err
		}, []*model.Field{pPaid, shown}},
		{"Note's type, its cells rewritten (UpdateField)", func() error {
			_, err := dataTable.UpdateField(ctx, vendors.Id, note.Id, dataTable.FieldPatch{Type: str(model.FieldNumber)}, owner)
			return err
		}, []*model.Field{label}},
		{"Note deleted (DeleteField)", func() error { return dataTable.DeleteField(ctx, vendors.Id, note.Id, owner) }, []*model.Field{label}},
		{"Note restored from the trash (RestoreFromTrash)", func() error {
			_, err := dataTable.RestoreFromTrash(ctx, vendors.Id, dataTable.RestoreInput{Fields: []uuid.UUID{note.Id}}, owner)
			return err
		}, []*model.Field{label}},
		{"an import into Vendors whose Stage isn't one of its options, which Stage gains (ImportBaseAs, Into)", func() error {
			_, err := dataTable.ImportBaseAs(ctx, owner, dataTable.ImportBase{Tables: []dataTable.ImportTable{{
				Key: "v", Name: vendors.Name, Into: vendors.Id,
				Fields: []dataTable.ImportField{{Key: "fStage", Name: "Stage", Type: model.FieldSelect, Into: stage.Id}},
				Rows: func(page func([]dataTable.ImportRow) error) error {
					return page([]dataTable.ImportRow{{Key: "r1", Cells: map[string]any{"fStage": "Closed"}}})
				},
			}}})
			return err
		}, []*model.Field{label}},
		{"Supplier made two-way: Vendors gets its other side (UpdateField, the inverse config)", func() error {
			_, err := dataTable.UpdateField(ctx, clients.Id, supplier.Id, dataTable.FieldPatch{Config: relation(vendors, true)}, owner)
			return err
		}, []*model.Field{shown}},
		{"Supplier deleted: its other side in Vendors goes with it (DeleteField, unlinkPair)", func() error {
			return dataTable.DeleteField(ctx, clients.Id, supplier.Id, owner)
		}, []*model.Field{shown}},
		{"Amount renamed", func() error {
			_, err := dataTable.UpdateField(ctx, vendors.Id, amount.Id, dataTable.FieldPatch{Name: str("Amount due")}, owner)
			return err
		}, nil},
		{"Amount moved", func() error {
			_, err := dataTable.UpdateField(ctx, vendors.Id, amount.Id, dataTable.FieldPatch{Position: &far}, owner)
			return err
		}, nil},
		{"Amount described", func() error {
			_, err := dataTable.UpdateField(ctx, vendors.Id, amount.Id, dataTable.FieldPatch{Description: str("What we owe them")}, owner)
			return err
		}, nil},
	}
	for _, l := range legs {
		before := gens()
		if err := l.write(); err != nil {
			t.Fatalf("%s: %v", l.name, err)
		}
		after := gens()
		for _, f := range watched {
			want := false
			for _, r := range l.rises {
				want = want || r.Id == f.Id
			}
			if rose := after[f.Id] > before[f.Id]; rose != want {
				t.Errorf("%s: %s's generation went from %d to %d (should rise: %v)", l.name, f.Name, before[f.Id], after[f.Id], want)
			}
		}
	}
}
