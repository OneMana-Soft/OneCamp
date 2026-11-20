//go:build integration

package business

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// linkedWorld is a workspace of linked tables, for what the rows page and
// the writes compute through relations: orders, vendors, projects and a
// private table the member cannot read.
//
// Vendors has three relations (its orders and projects, both the other
// sides of two-way relations, and a partner: a relation to itself) and
// three rollups over its orders (a sum, a list and a count with a
// condition). Projects rolls up its vendors' rollup (a rollup of a
// rollup), counts its links to the private table and has a formula over
// its vendors' spend.
type linkedWorld struct {
	w                                 *relationWorld
	orders, vendors, projects, secret *model.DataTable
	oName, vName, pName               *model.Field
	amount, status, vendor, project   *model.Field
	partner, pSecret                  *model.Field
	vendorRows, projectRows           []uuid.UUID
	hidden                            uuid.UUID // the private table's row
	// names is every id the fixture made, by its name, so a page reads the
	// same in any run.
	names map[string]string
}

func newLinkedWorld(t *testing.T, vendors, ordersEach int) *linkedWorld {
	w := newRelationWorld(t)
	f := &linkedWorld{w: w, names: map[string]string{}}
	f.orders, f.oName = w.table("Orders")
	f.vendors, f.vName = w.table("Vendors")
	f.projects, f.pName = w.table("Projects")
	secret, err := CreateTable(w.ctx, TableInput{Name: "Secret", Visibility: "private"}, w.me)
	if err != nil {
		t.Fatalf("the private table: %v", err)
	}
	f.secret = secret
	sFields, _ := model.ListFields(w.ctx, secret.Id)
	twoWay := func(to *model.DataTable) map[string]interface{} {
		return map[string]interface{}{"relation_target": "table", "table_id": to.Id.String(), "two_way": true}
	}
	oneWay := func(to *model.DataTable) map[string]interface{} {
		return map[string]interface{}{"relation_target": "table", "table_id": to.Id.String(), "two_way": false}
	}
	rollup := func(relation, of *model.Field, how string) map[string]interface{} {
		cfg := map[string]interface{}{"relation": relation.Id.String(), "aggregate": how}
		if of != nil {
			cfg["field"] = of.Id.String()
		}
		return cfg
	}
	f.amount = w.field(f.orders, "Amount", model.FieldNumber, nil)
	f.status = w.field(f.orders, "Status", model.FieldText, nil)
	f.vendor = w.field(f.orders, "Vendor", model.FieldRelation, twoWay(f.vendors))
	vOrders := w.otherSide(f.vendors, f.vendor)
	f.project = w.field(f.projects, "Vendors", model.FieldRelation, twoWay(f.vendors))
	w.otherSide(f.vendors, f.project)
	f.partner = w.field(f.vendors, "Partner", model.FieldRelation, oneWay(f.vendors))
	spend := w.field(f.vendors, "Spend", model.FieldRollup, rollup(vOrders, f.amount, "sum"))
	w.field(f.vendors, "Order names", model.FieldRollup, rollup(vOrders, f.oName, "list"))
	open := rollup(vOrders, nil, "count")
	open["where"] = []interface{}{map[string]interface{}{"field": f.status.Id.String(), "op": "eq", "value": "Open"}}
	w.field(f.vendors, "Open orders", model.FieldRollup, open)
	w.field(f.projects, "Total spend", model.FieldRollup, rollup(f.project, spend, "sum"))
	f.pSecret = w.field(f.projects, "Secret", model.FieldRelation, oneWay(secret))
	w.field(f.projects, "Secrets", model.FieldRollup, rollup(f.pSecret, nil, "count"))
	w.field(f.projects, "Doubled", model.FieldFormula, map[string]interface{}{"formula": "SUM({Vendors}.{Spend}) * 2"})

	for _, tb := range []*model.DataTable{f.orders, f.vendors, f.projects, secret} {
		f.names[tb.Id.String()] = "table " + tb.Name
		fields, _ := model.ListFields(w.ctx, tb.Id)
		for _, fl := range fields {
			f.names[fl.Id.String()] = "field " + tb.Name + "." + fl.Name
		}
	}
	named := func(tb *model.DataTable, name *model.Field, label string, values map[string]interface{}) uuid.UUID {
		if values == nil {
			values = map[string]interface{}{}
		}
		values[name.Id.String()] = label
		id := w.row(tb, values)
		f.names[id.String()] = "row " + label
		return id
	}
	hidden := named(secret, sFields[0], "Hidden", nil)
	f.hidden = hidden
	// Each vendor's partner is the one before it (a link to its own table).
	for i := range vendors {
		values := map[string]interface{}{}
		if i > 0 {
			values[f.partner.Id.String()] = []interface{}{f.vendorRows[i-1].String()}
		}
		f.vendorRows = append(f.vendorRows, named(f.vendors, f.vName, fmt.Sprintf("Vendor %02d", i+1), values))
	}
	gone := named(f.vendors, f.vName, "Gone", nil)
	for i, v := range f.vendorRows {
		for j := range ordersEach {
			status := "Closed"
			if (i+j)%2 == 0 {
				status = "Open"
			}
			links := []interface{}{v.String()}
			if i == 0 && j == 0 {
				links = append(links, gone.String())
			}
			named(f.orders, f.oName, fmt.Sprintf("Order %02d-%02d", i+1, j+1), map[string]interface{}{
				f.amount.Id.String(): (i+1)*100 + j*7, f.status.Id.String(): status, f.vendor.Id.String(): links,
			})
		}
	}
	for i := range max(1, vendors/3) {
		links := []interface{}{}
		for k := i * 3; k < min(len(f.vendorRows), i*3+3); k++ {
			links = append(links, f.vendorRows[k].String())
		}
		if i == 0 {
			links = append(links, gone.String())
		}
		values := map[string]interface{}{f.project.Id.String(): links}
		if i == 0 {
			values[f.pSecret.Id.String()] = []interface{}{hidden.String()}
		}
		f.projectRows = append(f.projectRows, named(f.projects, f.pName, fmt.Sprintf("Project %02d", i+1), values))
	}
	// A link to a row deleted since: an order and a project link to it.
	if _, err := DeleteRow(w.ctx, f.vendors.Id, gone, w.me); err != nil {
		t.Fatalf("deleting a linked vendor: %v", err)
	}
	return f
}

// page is a table's first window of rows, as actor reads it on the rows
// page, by name: each row's cells by field name, ids put as the names the
// fixture gave them.
func (f *linkedWorld) page(tb *model.DataTable, actor Actor) map[string]interface{} {
	f.w.t.Helper()
	win, err := QueryRows(f.w.ctx, tb.Id, actor, query.Spec{}, query.Page{Size: 200}, true)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	return map[string]interface{}{"rows": f.byName(win.Rows), "total": win.Total}
}

// guestPage is a table's rows read as a guest (asGuest), the way an AI
// column's prompt reads a row: links and rollups only within the table.
func (f *linkedWorld) guestPage(tb *model.DataTable) map[string]interface{} {
	f.w.t.Helper()
	fields, err := model.ListFields(f.w.ctx, tb.Id)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	rows, err := model.ListRows(f.w.ctx, tb.Id, 200, 0)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	withComputed(asGuest(f.w.ctx), fields, rows)
	return map[string]interface{}{"rows": f.byName(rows)}
}

// byName is each row's cells by field name, ids put as the fixture's names.
func (f *linkedWorld) byName(rs []*model.Row) []interface{} {
	rows := []interface{}{}
	for _, r := range rs {
		cells := map[string]interface{}{}
		for field, v := range parseRowValues(r.Values) {
			cells[f.name(field)] = f.named(v)
		}
		rows = append(rows, map[string]interface{}{"row": f.name(r.Id.String()), "cells": cells})
	}
	return rows
}

var anId = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

func (f *linkedWorld) name(id string) string {
	if n, ok := f.names[id]; ok {
		return n
	}
	return anId.ReplaceAllString(id, "<an id the fixture did not make>")
}

// named is v with every id in it put as its name.
func (f *linkedWorld) named(v interface{}) interface{} {
	switch x := v.(type) {
	case string:
		return anId.ReplaceAllStringFunc(x, f.name)
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, e := range x {
			out[i] = f.named(e)
		}
		return out
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, e := range x {
			out[anId.ReplaceAllStringFunc(k, f.name)] = f.named(e)
		}
		return out
	}
	return v
}

const h7Golden = "testdata/h7-golden.json"

// The rows page's formula, rollup and relation values over linked tables,
// for the owner, for a member who cannot read the private table and for a
// guest (who opens only the table it reads), are the ones
// testdata/h7-golden.json holds, byte for byte: read on the code before H7
// batched the linked reads (H7_GOLDEN_WRITE=1 writes it).
func TestLinkedValuesHoldTheirGolden(t *testing.T) {
	f := newLinkedWorld(t, 6, 3)
	got := map[string]interface{}{}
	tables := []*model.DataTable{f.orders, f.vendors, f.projects, f.secret}
	for _, who := range []struct {
		name  string
		actor Actor
	}{{"owner", f.w.me}, {"member", f.w.member}} {
		for _, tb := range tables {
			got[who.name+" reads "+tb.Name] = f.page(tb, who.actor)
		}
	}
	for _, tb := range tables {
		got["guest reads "+tb.Name] = f.guestPage(tb)
	}
	out, err := json.MarshalIndent(got, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, '\n')
	if os.Getenv("H7_GOLDEN_WRITE") == "1" {
		if err := os.MkdirAll(filepath.Dir(h7Golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h7Golden, out, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", h7Golden, len(out))
		return
	}
	want, err := os.ReadFile(h7Golden)
	if err != nil {
		t.Fatalf("the golden: %v", err)
	}
	if string(want) != string(out) {
		_ = os.WriteFile(h7Golden+".got", out, 0o644)
		t.Fatalf("the linked values moved from %s: compare it with %s.got", h7Golden, h7Golden)
	}
}
