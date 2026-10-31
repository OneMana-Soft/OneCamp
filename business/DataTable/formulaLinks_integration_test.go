//go:build integration

package business

// {Link}.{Field} in formulas end to end (tables B2): a formula reads a field
// of the rows a link links to as a list, from either side, through ARRAY
// functions, FILTER and SUM; it's stored by id, shown by name, and refused
// where a rollup would be.
//
// Run: go test -tags=integration ./business/DataTable/ -run 'TestFormulasReadLinkedFields' -v

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func TestFormulasReadLinkedFields(t *testing.T) {
	w := newRelationWorld(t)
	vendors, vName := w.table("Vendors")
	budget, bName := w.table("Budget")
	city := w.field(vendors, "City", model.FieldText, nil)
	cost := w.field(budget, "Cost", model.FieldNumber, nil)
	vendor := w.field(budget, "Vendor", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String(), "two_way": true})
	other := w.otherSide(vendors, vendor)

	cities := w.field(budget, "Cities", model.FieldFormula, map[string]interface{}{"formula": `ARRAYJOIN(ARRAYUNIQUE({Vendor}.{City}), " / ")`})
	spend := w.field(vendors, "Spend", model.FieldFormula, map[string]interface{}{"formula": "SUM({" + other.Name + "}.{Cost})"})
	big := w.field(vendors, "Big", model.FieldFormula, map[string]interface{}{"formula": "COUNTA(FILTER({" + other.Name + "}.{Cost}, CURRENT() >= 100))"})

	acme := w.row(vendors, map[string]interface{}{vName.Id.String(): "Acme", city.Id.String(): "Pune"})
	globex := w.row(vendors, map[string]interface{}{vName.Id.String(): "Globex", city.Id.String(): "Delhi"})
	booth := w.row(budget, map[string]interface{}{bName.Id.String(): "Booth", cost.Id.String(): 850, vendor.Id.String(): []interface{}{acme.String(), globex.String()}})
	w.row(budget, map[string]interface{}{bName.Id.String(): "Film", cost.Id.String(): 40, vendor.Id.String(): []interface{}{acme.String()}})

	if got := w.cells(budget, &w.me)[booth][cities.Id.String()]; got != "Pune / Delhi" {
		t.Errorf("Booth's cities: %#v", got)
	}
	v := w.cells(vendors, &w.me)
	if got := v[acme][spend.Id.String()]; got != float64(890) {
		t.Errorf("Acme's spend: %#v", got)
	}
	if got := v[acme][big.Id.String()]; got != float64(1) {
		t.Errorf("Acme's big items: %#v", got)
	}
	if got := v[globex][spend.Id.String()]; got != float64(850) {
		t.Errorf("Globex's spend: %#v", got)
	}

	// Stored by id, so renaming the linked field keeps it, and shown by name.
	fs, _ := model.ListFields(w.ctx, budget.Id)
	for _, f := range fs {
		if f.Id == cities.Id && !strings.Contains(f.Config, "{#"+vendor.Id.String()+"}.{#"+city.Id.String()+"}") {
			t.Errorf("stored as %s", f.Config)
		}
	}
	town := "Town"
	if _, err := UpdateField(w.ctx, vendors.Id, city.Id, FieldPatch{Name: &town}, w.me); err != nil {
		t.Fatalf("rename: %v", err)
	}
	b, err := GetBundle(w.ctx, budget.Id, w.me)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range b.Fields {
		if f.Id == cities.Id && !strings.Contains(f.Config, `{Vendor}.{Town}`) {
			t.Errorf("shown as %s", f.Config)
		}
	}
	if got := w.cells(budget, &w.me)[booth][cities.Id.String()]; got != "Pune / Delhi" {
		t.Errorf("after the rename: %#v", got)
	}

	// Where a rollup can't add up, a formula can't read: links to another
	// table, a field that isn't there, a field that isn't a link.
	refusals := map[string]string{
		"ARRAYJOIN({" + other.Name + "}.{Vendor})": "A formula can't read links to another table",
		"{Vendor}.{Owner}":                         `There's no field called "Owner" in the rows {Vendor} links to`,
		"{Cost}.{City}":                            "{Cost} doesn't link to a table's rows",
	}
	for src, want := range refusals {
		table := budget
		if strings.Contains(src, other.Name) {
			table = vendors
		}
		f, err := CreateField(w.ctx, table.Id, FieldInput{Name: "Try " + uuid.NewString()[:6], Type: model.FieldFormula, Config: map[string]interface{}{"formula": src}}, w.me)
		if err != nil {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %v, want %q", src, err, want)
			}
			continue
		}
		// Saved: the cells say why.
		for _, c := range w.cells(table, &w.me) {
			e, _ := c[f.Id.String()].(map[string]interface{})
			if why, _ := e["error"].(string); !strings.Contains(why, want) {
				t.Errorf("%s gives %#v, want %q", src, c[f.Id.String()], want)
			}
			break
		}
	}
}

// A rollup adds up by a formula over values and only the linked rows that
// meet its conditions, with a result format kept as saved; its conditions
// are stored by field id.
func TestRollupsWithFormulasAndConditions(t *testing.T) {
	w := newRelationWorld(t)
	vendors, vName := w.table("Vendors")
	orders, oName := w.table("Orders")
	amount := w.field(orders, "Amount", model.FieldNumber, nil)
	status := w.field(orders, "Status", model.FieldText, nil)
	vendor := w.field(orders, "Vendor", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String(), "two_way": true})
	other := w.otherSide(vendors, vendor)
	openSpend := w.field(vendors, "Open spend", model.FieldRollup, map[string]interface{}{
		"relation": other.Id.String(), "field": amount.Id.String(), "aggregate": "formula", "formula": "SUM(values) * 1.18",
		"where":  []interface{}{map[string]interface{}{"field": "Status", "op": "eq", "value": "Open"}},
		"format": map[string]interface{}{"currency": "INR", "decimals": 2},
	})
	openCount := w.field(vendors, "Open orders", model.FieldRollup, map[string]interface{}{
		"relation": other.Id.String(), "aggregate": "count",
		"where": []interface{}{map[string]interface{}{"field": status.Id.String(), "op": "eq", "value": "Open"}},
	})
	acme := w.row(vendors, map[string]interface{}{vName.Id.String(): "Acme"})
	for i, o := range []struct {
		amount float64
		status string
	}{{100, "Open"}, {250, "Closed"}, {50, "Open"}} {
		w.row(orders, map[string]interface{}{oName.Id.String(): string(rune('A' + i)), amount.Id.String(): o.amount, status.Id.String(): o.status, vendor.Id.String(): []interface{}{acme.String()}})
	}
	c := w.cells(vendors, &w.me)[acme]
	if got := c[openSpend.Id.String()]; got != float64(177) {
		t.Errorf("open spend: %#v", got)
	}
	if got := c[openCount.Id.String()]; got != float64(2) {
		t.Errorf("open orders: %#v", got)
	}
	fs, _ := model.ListFields(w.ctx, vendors.Id)
	for _, f := range fs {
		if f.Id != openSpend.Id {
			continue
		}
		var cfg rollupConfig
		var raw struct {
			Format map[string]interface{} `json:"format"`
		}
		_ = json.Unmarshal([]byte(f.Config), &cfg)
		_ = json.Unmarshal([]byte(f.Config), &raw)
		if len(cfg.Where) != 1 || cfg.Where[0].Field != status.Id.String() || raw.Format["currency"] != "INR" {
			t.Errorf("stored as %s", f.Config)
		}
	}
	// What can't be a condition or a formula isn't saved.
	for what, cfg := range map[string]map[string]interface{}{
		"a condition on a link":  {"relation": other.Id.String(), "aggregate": "count", "where": []interface{}{map[string]interface{}{"field": vendor.Id.String(), "op": "not_empty"}}},
		"an unclosed formula":    {"relation": other.Id.String(), "field": amount.Id.String(), "aggregate": "formula", "formula": "SUM(values"},
		"a format with a colour": {"relation": other.Id.String(), "aggregate": "count", "format": map[string]interface{}{"colour": "red"}},
	} {
		if _, err := CreateField(w.ctx, vendors.Id, FieldInput{Name: "Try " + uuid.NewString()[:6], Type: model.FieldRollup, Config: cfg}, w.me); err == nil {
			t.Errorf("%s was saved", what)
		}
	}
}

// A rollup orders the linked rows it adds up by one of their fields, either
// way, and adds up its first N (airtable-02): saved with the sort by field
// id, worked out on read, and refused where it can't sort by that field or N
// isn't a whole number from 1 to the most a rollup adds up.
func TestRollupsSortAndTakeTheirFirstSaved(t *testing.T) {
	w := newRelationWorld(t)
	vendors, vName := w.table("Vendors")
	orders, oName := w.table("Orders")
	amount := w.field(orders, "Amount", model.FieldNumber, nil)
	placed := w.field(orders, "Placed", model.FieldDate, nil)
	vendor := w.field(orders, "Vendor", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String(), "two_way": true})
	other := w.otherSide(vendors, vendor)
	latest := w.field(vendors, "Latest two", model.FieldRollup, map[string]interface{}{
		"relation": other.Id.String(), "field": oName.Id.String(), "aggregate": "list", "sort": "Placed", "desc": true, "limit": 2,
	})
	firstSpend := w.field(vendors, "First two spend", model.FieldRollup, map[string]interface{}{
		"relation": other.Id.String(), "field": amount.Id.String(), "aggregate": "sum", "sort": placed.Id.String(), "limit": 2,
	})
	// The tables' one order (RowQuery, as a view sorted by the field shows
	// it): a choice by its options' order, a numbered text as people count.
	priority := w.field(orders, "Priority", model.FieldSelect, map[string]interface{}{"options": []map[string]interface{}{{"label": "High"}, {"label": "Medium"}, {"label": "Low"}}})
	ref := w.field(orders, "Ref", model.FieldText, nil)
	tags := w.field(orders, "Tags", model.FieldMultiSelect, map[string]interface{}{"options": []map[string]interface{}{{"label": "Rush"}}})
	urgent := w.field(vendors, "Most urgent two", model.FieldRollup, map[string]interface{}{
		"relation": other.Id.String(), "field": oName.Id.String(), "aggregate": "list", "sort": priority.Id.String(), "limit": 2,
	})
	byRef := w.field(vendors, "By ref", model.FieldRollup, map[string]interface{}{
		"relation": other.Id.String(), "field": oName.Id.String(), "aggregate": "list", "sort": "Ref",
	})
	acme := w.row(vendors, map[string]interface{}{vName.Id.String(): "Acme"})
	// Linked in this order; D has no date and no priority, so it sorts last either way.
	for _, o := range []struct {
		name             string
		amount           float64
		placed, priority string
		ref              string
	}{{"A", 100, "2026-10-02", "Low", "PO 10"}, {"B", 250, "2026-10-09", "High", "PO 9"}, {"C", 50, "2026-10-05", "Medium", "PO 100"}, {"D", 75, "", "", "PO 1"}} {
		cells := map[string]interface{}{oName.Id.String(): o.name, amount.Id.String(): o.amount, ref.Id.String(): o.ref, vendor.Id.String(): []interface{}{acme.String()}}
		if o.placed != "" {
			cells[placed.Id.String()] = o.placed
		}
		if o.priority != "" {
			cells[priority.Id.String()] = o.priority
		}
		w.row(orders, cells)
	}
	c := w.cells(vendors, &w.me)[acme]
	if got := c[latest.Id.String()]; got != "B, C" {
		t.Errorf("the latest two orders: %#v", got)
	}
	if got := c[firstSpend.Id.String()]; got != float64(150) {
		t.Errorf("the first two orders' spend: %#v", got)
	}
	if got := c[urgent.Id.String()]; got != "B, C" {
		t.Errorf("the two most urgent orders, High then Medium: %#v", got)
	}
	if got := c[byRef.Id.String()]; got != "D, B, A, C" {
		t.Errorf("the orders by ref, PO 1, 9, 10, 100: %#v", got)
	}
	fs, _ := model.ListFields(w.ctx, vendors.Id)
	for _, f := range fs {
		if f.Id != latest.Id {
			continue
		}
		var cfg struct {
			Sort  string `json:"sort"`
			Desc  bool   `json:"desc"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal([]byte(f.Config), &cfg); err != nil || cfg.Sort != placed.Id.String() || !cfg.Desc || cfg.Limit != 2 {
			t.Errorf("stored as %s", f.Config)
		}
	}
	for what, cfg := range map[string]map[string]interface{}{
		"a sort by a link":                   {"sort": vendor.Id.String()},
		"a sort by a list of choices":        {"sort": tags.Id.String()},
		"a sort by a field that isn't there": {"sort": "Owner"},
		"no rows":                            {"limit": 0},
		"part of a row":                      {"limit": 2.5},
		"more than a rollup adds up":         {"limit": maxRolledUp + 1},
		"a number in words":                  {"limit": "two"},
		"a sort that isn't a field's name":   {"sort": 3},
	} {
		cfg["relation"], cfg["field"], cfg["aggregate"] = other.Id.String(), amount.Id.String(), "sum"
		if _, err := CreateField(w.ctx, vendors.Id, FieldInput{Name: "Try " + uuid.NewString()[:6], Type: model.FieldRollup, Config: cfg}, w.me); err == nil {
			t.Errorf("%s was saved", what)
		}
	}
}

// A count of its first N says N for a row with more links than a cell shows,
// when no other rollup adds up that link's rows (airtable-02): the read
// fetches only the links a cell shows, and a count needs no more.
func TestRollupCountsItsFirstNPastWhatACellShows(t *testing.T) {
	w := newRelationWorld(t)
	depots, dName := w.table("Depots")
	parcels, pName := w.table("Parcels")
	depot := w.field(parcels, "Depot", model.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": depots.Id.String(), "two_way": true})
	other := w.otherSide(depots, depot)
	firstFive := w.field(depots, "First five", model.FieldRollup, map[string]interface{}{"relation": other.Id.String(), "aggregate": "count", "limit": 5})
	busy := w.row(depots, map[string]interface{}{dName.Id.String(): "Busy"})
	for i := 0; i <= maxShownLinks; i++ {
		w.row(parcels, map[string]interface{}{pName.Id.String(): fmt.Sprint("P", i), depot.Id.String(): []interface{}{busy.String()}})
	}
	if got := w.cells(depots, &w.me)[busy][firstFive.Id.String()]; got != float64(5) {
		t.Errorf("the first five of %d parcels: %#v", maxShownLinks+1, got)
	}
}
