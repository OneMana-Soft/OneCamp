//go:build integration

package business_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// airtable-03: a kept value is what a read works out, after every write. A
// table made through the API is filled by the sweep; a write two levels down
// (Ledger, under Vendors.Paid, under Projects.Paid) marks the kept values it
// changes and the next read works them out first; a link change does the
// same; a change to a field makes it and every kept field reaching it stale
// until the sweep fills them again, and a read meanwhile is worked out. A
// formula reading the clock is never kept, and is kept once it stops reading
// it. A field deleted and brought back makes the formulas reading it stale
// each time, and they're kept again once it's back. None of these field
// changes records a change set. At every step a member who can't open Ledger
// is answered by their own read (Paid can't be worked out for them), never in
// the kept order.
//
// Run: integ_rel.sh <worktree> <log> '^(TestKeptValuesFollowEveryWrite)$' with PKGS='./business/RowQuery/'
func TestKeptValuesFollowEveryWrite(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.SetupDgraph(t)
	db := postgresInit.DBConn.SqlDB
	me, member := uuid.New(), uuid.New()
	for _, u := range []struct {
		id   uuid.UUID
		name string
	}{{me, "kept-owner"}, {member, "kept-member"}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2 || '@example.test', $2, $2, NOW(), NOW())`, u.id, u.name); err != nil {
			t.Fatal(err)
		}
	}
	owner := dataTable.Actor{UserID: me}
	table := func(name, visibility string) *model.DataTable {
		tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: name, Visibility: visibility}, owner)
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
	row := func(tb *model.DataTable, vals map[string]interface{}) uuid.UUID {
		r, err := dataTable.CreateRow(ctx, tb.Id, dataTable.RowInput{Values: vals}, owner)
		if err != nil {
			t.Fatal(err)
		}
		return r.Id
	}
	links := func(tb *model.DataTable, r uuid.UUID, f *model.Field, add, remove []uuid.UUID) {
		if _, _, err := dataTable.ChangeLinks(ctx, tb.Id, r, f.Id, add, remove, owner); err != nil {
			t.Fatal(err)
		}
	}
	relation := func(to *model.DataTable) map[string]interface{} {
		return map[string]interface{}{"relation_target": "table", "table_id": to.Id.String(), "two_way": false}
	}
	rollup := func(rel, of *model.Field, how string) map[string]interface{} {
		return map[string]interface{}{"relation": rel.Id.String(), "field": of.Id.String(), "aggregate": how}
	}

	ledger := table("Ledger", model.VisibilityPrivate)
	amount := add(ledger, "Amount", model.FieldNumber, nil)
	vendors := table("Vendors", "workspace")
	vLedger := add(vendors, "Ledger", model.FieldRelation, relation(ledger))
	vPaid := add(vendors, "Paid", model.FieldRollup, rollup(vLedger, amount, "sum"))
	projects := table("Projects", "workspace")
	budget := add(projects, "Budget", model.FieldNumber, nil)
	hours := add(projects, "Hours", model.FieldNumber, nil)
	pVendors := add(projects, "Vendors", model.FieldRelation, relation(vendors))
	pPaid := add(projects, "Paid", model.FieldRollup, rollup(pVendors, vPaid, "sum"))
	double := add(projects, "Double", model.FieldFormula, map[string]interface{}{"formula": "{Budget} * 2"})
	today := add(projects, "Today", model.FieldFormula, map[string]interface{}{"formula": "TODAY()"})

	// Ledger 100, 200, 400, 800; V1 = L1, V2 = L2 + L3, V3 = L4; P1 = V1, P2 = V2, P3 = V3.
	var l []uuid.UUID
	for _, a := range []float64{100, 200, 400, 800} {
		l = append(l, row(ledger, map[string]interface{}{amount.Id.String(): a}))
	}
	v1, v2, v3 := row(vendors, nil), row(vendors, nil), row(vendors, nil)
	links(vendors, v1, vLedger, l[:1], nil)
	links(vendors, v2, vLedger, l[1:3], nil)
	links(vendors, v3, vLedger, l[3:], nil)
	p1 := row(projects, map[string]interface{}{budget.Id.String(): 30, hours.Id.String(): 3})
	p2 := row(projects, map[string]interface{}{budget.Id.String(): 10, hours.Id.String(): 1})
	p3 := row(projects, map[string]interface{}{budget.Id.String(): 20, hours.Id.String(): 2})
	links(projects, p1, pVendors, []uuid.UUID{v1}, nil)
	links(projects, p2, pVendors, []uuid.UUID{v2}, nil)
	links(projects, p3, pVendors, []uuid.UUID{v3}, nil)

	// current is whether the store holds f, and whether f's entries hold its
	// values now: filled at its generation, as a kind of value.
	current := func(f *model.Field) (held, ok bool) {
		var gen, filled int64
		var kind string
		err := db.QueryRowContext(ctx, `SELECT gen, filled_gen, kind FROM data_table_computed WHERE field_id = $1`, f.Id).Scan(&gen, &filled, &kind)
		if errors.Is(err, sql.ErrNoRows) {
			return false, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return true, filled == gen && kind != ""
	}
	sweep := func() {
		t.Helper()
		if err := rowQuery.RecomputeKept(ctx); err != nil {
			t.Fatal(err)
		}
	}
	keptValue := func(f *model.Field, r uuid.UUID) (n float64, ok bool) {
		err := db.QueryRowContext(ctx, `SELECT n FROM data_table_cell_index WHERE field_id = $1 AND row_id = $2 AND n IS NOT NULL`, f.Id, r).Scan(&n)
		return n, err == nil
	}
	// waiting is how many of fs's kept values wait to be worked out again
	// (every field's, with none named).
	waiting := func(fs ...*model.Field) (n int) {
		count := func(q string, args ...any) {
			var c int
			if err := db.QueryRowContext(ctx, q, args...).Scan(&c); err != nil {
				t.Fatal(err)
			}
			n += c
		}
		if len(fs) == 0 {
			count(`SELECT count(*) FROM data_table_computed_dirty`)
		}
		for _, f := range fs {
			count(`SELECT count(*) FROM data_table_computed_dirty WHERE field_id = $1`, f.Id)
		}
		return n
	}
	scope := query.Scope{Kind: query.ScopeTable, ID: projects.Id.String()}
	order := func(who accessBusiness.Subject, by *model.Field) ([]uuid.UUID, error) {
		res, err := query.Queries.Run(ctx, who, query.Spec{Scope: scope, Sorts: []query.Sort{{Field: "field:" + by.Id.String(), Desc: true}}}, query.Page{Size: 10})
		var ids []uuid.UUID
		for _, raw := range res.IDs {
			ids = append(ids, uuid.MustParse(raw))
		}
		return ids, err
	}
	asOwner := accessBusiness.Subject{UserUUID: me.String()}
	asMember := accessBusiness.Subject{UserUUID: member.String()}
	names := map[uuid.UUID]string{p1: "P1", p2: "P2", p3: "P3"}
	say := func(ids []uuid.UUID) string {
		var out []string
		for _, id := range ids {
			out = append(out, names[id])
		}
		return fmt.Sprint(out)
	}
	// owns: the owner's order by Paid is want; the member's is never it.
	owns := func(step string, want ...uuid.UUID) {
		got, err := order(asOwner, pPaid)
		if err != nil || say(got) != say(want) {
			t.Errorf("%s: the owner by Paid reads %s (error %v), where %s is right", step, say(got), err, say(want))
		}
		theirs, err := order(asMember, pPaid)
		if err == nil && say(theirs) == say(want) {
			t.Errorf("%s: the member, who can't open Ledger, reads the kept order %s", step, say(theirs))
		} else if err == nil && say(theirs) != say([]uuid.UUID{p1, p2, p3}) {
			t.Errorf("%s: the member reads %s, where their own read gives the table's order", step, say(theirs))
		}
	}

	// The sweep fills a table made through the API, as it does every minute.
	sweep()
	for _, f := range []*model.Field{vPaid, pPaid, double} {
		if held, ok := current(f); !held || !ok {
			t.Errorf("%s isn't kept current after the sweep (held %v)", f.Name, held)
		}
	}
	if _, ok := current(today); ok {
		t.Errorf("Today reads the clock, but the store keeps it")
	}
	owns("filled", p3, p2, p1)
	if n, _ := keptValue(pPaid, p3); n != 800 {
		t.Errorf("P3's kept Paid is %v, where it adds up to 800", n)
	}

	// A write two levels down: L1 = 1000 marks V1 and P1; the next read works them out first.
	if _, err := dataTable.PatchRow(ctx, ledger.Id, l[0], map[string]interface{}{amount.Id.String(): 1000}, owner); err != nil {
		t.Fatal(err)
	}
	if waiting() == 0 {
		t.Errorf("a write in Ledger marked no kept value")
	}
	// The read of Projects works out the Paid values it orders by; Vendors'
	// own Paid values wait for a read of Vendors, or the sweep.
	owns("Ledger changed", p1, p3, p2)
	if n := waiting(pPaid); n != 0 {
		t.Errorf("%d of Projects' kept Paid values still wait after a read worked them out", n)
	}
	if n, _ := keptValue(pPaid, p1); n != 1000 {
		t.Errorf("P1's kept Paid is %v after Ledger changed, where it adds up to 1000", n)
	}

	// A link change: P2 takes V3 too (600 + 800).
	links(projects, p2, pVendors, []uuid.UUID{v3}, nil)
	owns("P2 linked V3", p2, p1, p3)

	// A change to a field: Vendors.Paid takes the largest. It and Projects.Paid,
	// which reaches Vendors, are stale until the sweep fills them again.
	if _, err := dataTable.UpdateField(ctx, vendors.Id, vPaid.Id, dataTable.FieldPatch{Config: rollup(vLedger, amount, "max")}, owner); err != nil {
		t.Fatal(err)
	}
	for _, f := range []*model.Field{vPaid, pPaid} {
		if _, ok := current(f); ok {
			t.Errorf("%s is still current after Vendors.Paid changed how it adds up", f.Name)
		}
	}
	owns("stale, worked out", p2, p1, p3) // P2 = 400 + 800, P1 = 1000, P3 = 800
	sweep()
	for _, f := range []*model.Field{vPaid, pPaid} {
		if _, ok := current(f); !ok {
			t.Errorf("%s isn't current after the sweep filled it again", f.Name)
		}
	}
	if n, _ := keptValue(pPaid, p2); n != 1200 {
		t.Errorf("P2's kept Paid is %v after the sweep, where it adds up to 1200", n)
	}
	owns("filled again", p2, p1, p3)

	// Today stops reading the clock: kept from the next fill.
	if _, err := dataTable.UpdateField(ctx, projects.Id, today.Id, dataTable.FieldPatch{Config: map[string]interface{}{"formula": "{Hours} + 1"}}, owner); err != nil {
		t.Fatal(err)
	}
	sweep()
	if _, ok := current(today); !ok {
		t.Errorf("Today reads Hours now, not the clock, but the sweep didn't keep it")
	}
	if n, _ := keptValue(today, p1); n != 4 {
		t.Errorf("P1's kept Today is %v, where Hours 3 gives 4", n)
	}

	// Hours deleted: Today, which reads it, is stale at once, and the sweep
	// can't keep it. Hours brought back: stale again until the sweep keeps it.
	if err := dataTable.DeleteField(ctx, projects.Id, hours.Id, owner); err != nil {
		t.Fatal(err)
	}
	if _, ok := current(today); ok {
		t.Errorf("Today is still current after Hours, which it reads, was deleted")
	}
	sweep()
	if _, ok := current(today); ok {
		t.Errorf("Today is kept while Hours, which it reads, is deleted")
	}
	if _, err := dataTable.RestoreFromTrash(ctx, projects.Id, dataTable.RestoreInput{Fields: []uuid.UUID{hours.Id}}, owner); err != nil {
		t.Fatal(err)
	}
	sweep()
	if _, ok := current(today); !ok {
		t.Errorf("Today isn't kept again after Hours came back and the sweep ran")
	}
	if n, _ := keptValue(today, p3); n != 3 {
		t.Errorf("P3's kept Today is %v after Hours came back, where Hours 2 gives 3", n)
	}
	owns("Hours back", p2, p1, p3)
}
