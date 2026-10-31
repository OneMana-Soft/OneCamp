//go:build integration

package business_test

// V-108 (main, 30 Oct): making any field on a table raised every kept
// formula and rollup of it, and every one reaching it (migration 535's
// trigger), and the sweep then filled every kept field of the table again
// over every row: at 100,000 rows, about 100 s of working-out for a new
// column none of them reads. A field written now raises only itself and the
// kept fields that read it, at every level, and a fill writes only the
// fields that aren't current. The test measures the work, not the time: the
// generations each write raised, and how many of each field's cell index
// entries the sweep wrote again.
//
// Run: integ23.sh <log> <worktree> './business/RowQuery/' 'TestAFieldWriteFillsOnlyTheFieldsThatReadIt'

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// fieldWriteRows is the Orders table's rows: enough that a field filled
// again shows as that many entries written.
const fieldWriteRows = 1200

func TestAFieldWriteFillsOnlyTheFieldsThatReadIt(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.SetupDgraph(t)
	db := postgresInit.DBConn.SqlDB
	me := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'field-writes@example.test', 'field-writes', 'Field writes', NOW(), NOW())`, me); err != nil {
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
	formulaOf := func(src string) map[string]interface{} { return map[string]interface{}{"formula": src} }

	// Orders: Double and Less (which reads Double) read Amount; Charged reads
	// Fee. Vendors adds up Orders' Double over its links.
	orders := table("Orders")
	amount := add(orders, "Amount", model.FieldNumber, nil)
	fee := add(orders, "Fee", model.FieldNumber, nil)
	double := add(orders, "Double", model.FieldFormula, formulaOf("{Amount} * 2"))
	charged := add(orders, "Charged", model.FieldFormula, formulaOf("{Fee} + 1"))
	less := add(orders, "Less", model.FieldFormula, formulaOf("{Double} - 1"))
	ids := make([]uuid.UUID, fieldWriteRows)
	for i := range ids {
		ids[i] = uuid.New()
	}
	fill(ctx, t, orders.Id, me, ids, func(i int) map[string]interface{} {
		return map[string]interface{}{amount.Id.String(): float64(i), fee.Id.String(): float64(i % 7)}
	})
	if err := rowQuery.IndexTable(ctx, orders.Id); err != nil {
		t.Fatal(err)
	}
	vendors := table("Vendors")
	link := add(vendors, "Orders", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": orders.Id.String(), "two_way": false})
	total := add(vendors, "Total", model.FieldRollup, map[string]interface{}{"relation": link.Id.String(), "field": double.Id.String(), "aggregate": "sum"})
	v, err := dataTable.CreateRow(ctx, vendors.Id, dataTable.RowInput{}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dataTable.ChangeLinks(ctx, vendors.Id, v.Id, link.Id, ids[:3], nil, owner); err != nil {
		t.Fatal(err)
	}

	kept := []*model.Field{double, charged, less, total}
	gens := func() map[uuid.UUID]int64 {
		out := map[uuid.UUID]int64{}
		for _, f := range kept {
			var gen int64
			if err := db.QueryRowContext(ctx, `SELECT gen FROM data_table_computed WHERE field_id = $1`, f.Id).Scan(&gen); err != nil {
				t.Fatalf("%s's generation: %v", f.Name, err)
			}
			out[f.Id] = gen
		}
		return out
	}
	// settle sweeps until every live kept field of both tables is current,
	// one just made included.
	settle := func(what string) {
		t.Helper()
		for pass := 0; ; pass++ {
			var all bool
			if err := db.QueryRowContext(ctx, `SELECT bool_and(c.filled_gen = c.gen) FROM data_table_computed c
				JOIN data_table_fields f ON f.id = c.field_id AND f.deleted_at IS NULL
				WHERE c.table_id = ANY($1)`, pq.Array([]uuid.UUID{orders.Id, vendors.Id})).Scan(&all); err != nil {
				t.Fatal(err)
			}
			if all {
				return
			}
			if pass == 20 {
				t.Fatalf("%s: the kept fields were never all filled", what)
			}
			if err := rowQuery.RecomputeKept(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	// since is the transaction id now: an entry written after it has a
	// later xmin.
	since := func() int64 {
		var x int64
		if err := db.QueryRowContext(ctx, `SELECT txid_current() % 4294967296`).Scan(&x); err != nil {
			t.Fatal(err)
		}
		return x
	}
	written := func(f *model.Field, after int64) int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM data_table_cell_index WHERE field_id = $1 AND xmin::text::bigint > $2`, f.Id, after).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	settle("the first fill")

	// step is one field write, then the sweep: it raises the generations of
	// the kept fields named in reads and of no other, and the sweep writes
	// entries again for those it can keep and for none of the rest.
	step := func(what string, write func(), reads ...*model.Field) {
		t.Helper()
		before, mark := gens(), since()
		write()
		after := gens()
		want := map[uuid.UUID]bool{}
		for _, f := range reads {
			want[f.Id] = true
		}
		var wrong []string
		for _, f := range kept {
			if raised := after[f.Id] != before[f.Id]; raised != want[f.Id] {
				wrong = append(wrong, fmt.Sprintf("%s raised %v", f.Name, raised))
			}
		}
		if len(wrong) > 0 {
			sort.Strings(wrong)
			t.Errorf("%s: wrong generations: %s (want raised: only the fields that read it)", what, strings.Join(wrong, ", "))
		}
		settle(what)
		var work []string
		for _, f := range kept {
			n := written(f, mark)
			if n > 0 && !want[f.Id] {
				work = append(work, fmt.Sprintf("%s %d", f.Name, n))
			}
		}
		if len(work) > 0 {
			sort.Strings(work)
			t.Errorf("%s: the sweep filled again fields that don't read it: %s entries written (want 0)", what, strings.Join(work, ", "))
		}
	}

	step("a plain field made", func() { add(orders, "Notes", model.FieldText, nil) })
	// The same write by the web's own path.
	step("a plain field made over HTTP (POST /tables/{id}/fields)", func() {
		rec := overHTTP(t, dataTableController.CreateField, "/tables/"+orders.Id.String()+"/fields", orders.Id, me, map[string]any{"name": "Memo", "type": model.FieldText})
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /tables/{id}/fields: %d %s", rec.Code, rec.Body.String())
		}
	})

	var triple *model.Field
	step("a formula made", func() { triple = add(orders, "Triple", model.FieldFormula, formulaOf("{Amount} * 3")) })
	kept = append(kept, triple)
	if n := written(triple, 0); n != fieldWriteRows {
		t.Errorf("Triple, made over %d rows, has %d entries after the sweep", fieldWriteRows, n)
	}

	step("Double's formula changed", func() {
		if _, err := dataTable.UpdateField(ctx, orders.Id, double.Id, dataTable.FieldPatch{Config: formulaOf("{Amount} * 4")}, owner); err != nil {
			t.Fatal(err)
		}
	}, double, less, total)

	step("Fee deleted", func() {
		if err := dataTable.DeleteField(ctx, orders.Id, fee.Id, owner); err != nil {
			t.Fatal(err)
		}
	}, charged)

	step("Fee brought back", func() {
		if _, err := dataTable.RestoreFromTrash(ctx, orders.Id, dataTable.RestoreInput{Fields: []uuid.UUID{fee.Id}}, owner); err != nil {
			t.Fatal(err)
		}
	}, charged)
	var n float64
	if err := db.QueryRowContext(ctx, `SELECT n FROM data_table_cell_index WHERE field_id = $1 AND row_id = $2`, charged.Id, ids[10]).Scan(&n); err != nil || n != 4 {
		t.Errorf("Charged on row 10 is kept as %v (%v) after Fee came back, where Fee 3 gives 4", n, err)
	}
}
