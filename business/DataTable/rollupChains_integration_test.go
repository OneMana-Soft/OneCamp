//go:build integration

package business

// Rollups of rollups end to end (airtable-02): a rollup adds up another
// table's rollup as that table works it out, sorted and cut to its first N
// like any field, and a formula reads one through a link. A chain more than
// maxRollupDepth deep, or one that comes back round, is refused when it's
// saved and says so when it's read; and a reader gets only what the tables
// they can open add up to.
//
// Run: go test -tags=integration ./business/DataTable/ -run 'TestRollupsOfRollups' -v

import (
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func TestRollupsOfRollups(t *testing.T) {
	w := newRelationWorld(t)
	orders, oName := w.table("Orders")
	vendors, vName := w.table("Vendors")
	projects, pName := w.table("Projects")
	portfolios, fName := w.table("Portfolios")
	twoWay := func(to *model.DataTable) map[string]interface{} {
		return map[string]interface{}{"relation_target": "table", "table_id": to.Id.String(), "two_way": true}
	}
	rollup := func(relation, of *model.Field, how string) map[string]interface{} {
		return map[string]interface{}{"relation": relation.Id.String(), "field": of.Id.String(), "aggregate": how}
	}
	amount := w.field(orders, "Amount", model.FieldNumber, nil)
	vendor := w.field(orders, "Vendor", model.FieldRelation, twoWay(vendors))
	vOrders := w.otherSide(vendors, vendor)
	spend := w.field(vendors, "Spend", model.FieldRollup, rollup(vOrders, amount, "sum"))
	names := w.field(vendors, "Order names", model.FieldRollup, rollup(vOrders, oName, "list"))
	pVendors := w.field(projects, "Vendors", model.FieldRelation, twoWay(vendors))
	vProjects := w.otherSide(vendors, pVendors)
	total := w.field(projects, "Total spend", model.FieldRollup, rollup(pVendors, spend, "sum"))
	biggest := w.field(projects, "Biggest vendor", model.FieldRollup, rollup(pVendors, spend, "max"))
	allOrders := w.field(projects, "All orders", model.FieldRollup, rollup(pVendors, names, "list"))
	firstByName := rollup(pVendors, spend, "sum")
	firstByName["sort"], firstByName["limit"] = vName.Id.String(), 1
	first := w.field(projects, "First vendor's spend", model.FieldRollup, firstByName)
	viaFormula := w.field(projects, "Spend by formula", model.FieldFormula, map[string]interface{}{"formula": "SUM({Vendors}.{Spend})"})
	fProjects := w.field(portfolios, "Projects", model.FieldRelation, twoWay(projects))
	grand := w.field(portfolios, "Grand total", model.FieldRollup, rollup(fProjects, total, "sum"))

	acme := w.row(vendors, map[string]interface{}{vName.Id.String(): "Acme"})
	bolt := w.row(vendors, map[string]interface{}{vName.Id.String(): "Bolt"})
	for _, o := range []struct {
		name   string
		amount float64
		to     uuid.UUID
	}{{"A", 100, acme}, {"B", 250, acme}, {"C", 50, bolt}} {
		w.row(orders, map[string]interface{}{oName.Id.String(): o.name, amount.Id.String(): o.amount, vendor.Id.String(): []interface{}{o.to.String()}})
	}
	// Bolt first: a rollup adds up in link order, unless it sorts.
	launch := w.row(projects, map[string]interface{}{pName.Id.String(): "Launch", pVendors.Id.String(): []interface{}{bolt.String(), acme.String()}})
	site := w.row(projects, map[string]interface{}{pName.Id.String(): "Site", pVendors.Id.String(): []interface{}{bolt.String()}})
	q4 := w.row(portfolios, map[string]interface{}{fName.Id.String(): "Q4", fProjects.Id.String(): []interface{}{launch.String(), site.String()}})

	p := w.cells(projects, &w.me)
	for what, c := range map[string]struct {
		got, want interface{}
	}{
		"Launch's total spend":           {p[launch][total.Id.String()], float64(400)},
		"Launch's biggest vendor":        {p[launch][biggest.Id.String()], float64(350)},
		"Launch's orders, vendor by one": {p[launch][allOrders.Id.String()], "C, A, B"},
		"Launch's first vendor by name":  {p[launch][first.Id.String()], float64(350)},
		"Launch's spend by formula":      {p[launch][viaFormula.Id.String()], float64(400)},
		"Site's total spend":             {p[site][total.Id.String()], float64(50)},
		"Q4's grand total, 3 deep":       {w.cells(portfolios, &w.me)[q4][grand.Id.String()], float64(450)},
	} {
		if c.got != c.want {
			t.Errorf("%s: %#v, want %#v", what, c.got, c.want)
		}
	}

	// A fourth rollup on top is one too deep.
	boards, _ := w.table("Boards")
	bPortfolios := w.field(boards, "Portfolios", model.FieldRelation, twoWay(portfolios))
	tooDeep := "A rollup can't add up rollups more than 3 levels deep"
	if _, err := CreateField(w.ctx, boards.Id, FieldInput{Name: "Everything", Type: model.FieldRollup, Config: rollup(bPortfolios, grand, "sum")}, w.me); err == nil || !strings.Contains(err.Error(), tooDeep) {
		t.Errorf("a rollup 4 deep: %v, want %q", err, tooDeep)
	}
	// Vendors' spend can't add up the projects' total, which adds it up.
	goesRound := "A rollup can't add up rollups that add each other up"
	if _, err := UpdateField(w.ctx, vendors.Id, spend.Id, FieldPatch{Config: rollup(vProjects, total, "sum")}, w.me); err == nil || !strings.Contains(err.Error(), goesRound) {
		t.Errorf("a rollup adding up what adds it up: %v, want %q", err, goesRound)
	}

	// (The AI edition also reads Projects for a second person here, as an agent
	// does; this edition has no agents.)

	// A chain that comes back round some other way (made before these
	// checks, or by two people at once) says so when it's read, and the read
	// ends.
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(w.ctx, `UPDATE data_table_fields SET config = $1 WHERE id = $2`,
		`{"relation":"`+vProjects.Id.String()+`","field":"`+total.Id.String()+`","aggregate":"sum"}`, spend.Id); err != nil {
		t.Fatalf("write the loop: %v", err)
	}
	p = w.cells(projects, &w.me)
	if why := errorOf(p[launch][total.Id.String()]); why != goesRound {
		t.Errorf("a loop read: the total says %q, want %q", why, goesRound)
	}
	if why := errorOf(w.cells(portfolios, &w.me)[q4][grand.Id.String()]); why == "" {
		t.Errorf("a loop read: the grand total adds up %#v", w.cells(portfolios, &w.me)[q4][grand.Id.String()])
	}
}

// errorOf is why a cell couldn't be worked out, or "".
func errorOf(cell interface{}) string {
	m, _ := cell.(map[string]interface{})
	why, _ := m["error"].(string)
	return why
}
