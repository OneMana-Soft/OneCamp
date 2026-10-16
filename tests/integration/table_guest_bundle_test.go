//go:build integration
// +build integration

package integration_test

// What a table's guest link carries, against Postgres 12 with every migration
// (TAB-BUG-5's amendment): every field's whole config, the AI columns'
// prompts among them, and who made the table and each row, which are member
// ids. A guest gets none of those; a member's bundle keeps them.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableGuestBundle -v

import (
	"encoding/json"
	"strings"
	"testing"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func TestTableGuestBundle(t *testing.T) {
	w := newTableWorld(t, "maya")
	maya := w.people["maya"]
	tb := w.table("maya", "Accounts")
	notes := w.field("maya", tb, "Notes", dataTableModel.FieldText, nil)
	w.field("maya", tb, "Risk", dataTableModel.FieldText,
		map[string]interface{}{"ai": map[string]interface{}{"prompt": "Rate the churn risk of {Notes} from our private board notes"}})
	if _, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{notes: "Renewal in March"}}, maya); err != nil {
		t.Fatal(err)
	}
	guest, err := dataTableBusiness.GetGuestBundle(w.ctx, tb.Id)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := json.Marshal(guest)
	// A field's config is a string in the bundle, so its keys are escaped.
	for _, leak := range []string{maya.UserID.String(), "created_by", "churn risk", `\"ai\":`} {
		if strings.Contains(string(text), leak) {
			t.Errorf("the guest bundle carries %q", leak)
		}
	}
	if !strings.Contains(string(text), `\"ai_filled\":true`) || !strings.Contains(string(text), "Renewal in March") {
		t.Errorf("the guest bundle is %s", text)
	}
	member, err := dataTableBusiness.GetBundle(w.ctx, tb.Id, maya)
	if err != nil {
		t.Fatal(err)
	}
	text, _ = json.Marshal(member)
	if !strings.Contains(string(text), `"created_by":"`+maya.UserID.String()) || !strings.Contains(string(text), "churn risk") {
		t.Errorf("the member's bundle lost what members see: %s", text)
	}

	// Deleting a column only marks it, so its cells stay stored, and every
	// read gave them out: a guest's, a member's and the AI's (the audit's
	// B2). Read with and without a computed column, which reads take
	// different ways.
	t.Run("a deleted column's cells reach no reader", func(t *testing.T) {
		fee := w.field("maya", tb, "Fee", dataTableModel.FieldText, nil)
		row, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{notes: "Upsell", fee: "£4,800 retainer"}}, maya)
		if err != nil {
			t.Fatal(err)
		}
		if err := dataTableBusiness.DeleteField(w.ctx, tb.Id, uuidOf(t, fee), maya); err != nil {
			t.Fatal(err)
		}
		reads := func(t *testing.T) map[string]string {
			t.Helper()
			guest, err := dataTableBusiness.GetGuestBundle(w.ctx, tb.Id)
			if err != nil {
				t.Fatal(err)
			}
			member, err := dataTableBusiness.GetBundle(w.ctx, tb.Id, maya)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := dataTableBusiness.ListRows(w.ctx, tb.Id, maya, 10, 0)
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]string{}
			for what, v := range map[string]interface{}{"the guest bundle": guest, "the member's bundle": member, "the rows endpoint": rows} {
				text, _ := json.Marshal(v)
				out[what] = string(text)
			}
			return out
		}
		for what, text := range reads(t) {
			if strings.Contains(text, "£4,800") || strings.Contains(text, fee) || !strings.Contains(text, "Upsell") {
				t.Errorf("%s: %s", what, text)
			}
		}
		w.field("maya", tb, "Loud", dataTableModel.FieldFormula, map[string]interface{}{"formula": "UPPER({Notes})"})
		for what, text := range reads(t) {
			if strings.Contains(text, "£4,800") || !strings.Contains(text, "UPSELL") {
				t.Errorf("with a formula, %s: %s", what, text)
			}
		}
		if v := w.stored(tb, row.Id); v[fee] != "£4,800 retainer" {
			t.Errorf("the deleted column's cell is stored as %v; reads leave it out, they don't delete it", v[fee])
		}
	})

	// A guest got every field's whole config and every saved view: another
	// table's id and name, a formula's text, a view's filters. Their view
	// reads how options and numbers show, and what a formula gives.
	t.Run("a guest gets the settings their view reads, and no others", func(t *testing.T) {
		vendors := w.table("maya", "Vendors under review")
		w.field("maya", tb, "Vendor", dataTableModel.FieldRelation, map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String()})
		w.field("maya", tb, "Echo", dataTableModel.FieldFormula, map[string]interface{}{"formula": `CONCATENATE({Notes}, LEFT("Quiet clause", 0))`})
		w.field("maya", tb, "Tier", dataTableModel.FieldSelect, map[string]interface{}{"options": []interface{}{map[string]interface{}{"label": "Gold", "color": "amber"}}, "tinted": true})
		w.field("maya", tb, "Fee", dataTableModel.FieldNumber, map[string]interface{}{"format": map[string]interface{}{"currency": "INR"}})
		if _, err := dataTableBusiness.CreateView(w.ctx, tb.Id, dataTableBusiness.ViewInput{Name: "Churn watch", Type: dataTableModel.ViewGrid,
			Config: map[string]interface{}{"filters": []interface{}{map[string]interface{}{"field": "Notes", "op": "contains", "value": "layoffs"}}}}, maya); err != nil {
			t.Fatal(err)
		}
		guest, err := dataTableBusiness.GetGuestBundle(w.ctx, tb.Id)
		if err != nil {
			t.Fatal(err)
		}
		text, _ := json.Marshal(guest)
		for _, leak := range []string{vendors.Id.String(), "Vendors under review", "Quiet clause", "Churn watch", "layoffs", "relation_target"} {
			if strings.Contains(string(text), leak) {
				t.Errorf("the guest bundle carries %q", leak)
			}
		}
		for _, kept := range []string{"Gold", "amber", `\"tinted\":true`, `\"currency\":\"INR\"`, `\"result\":\"text\"`, `\"ai_filled\":true`} {
			if !strings.Contains(string(text), kept) {
				t.Errorf("the guest bundle lost %s: %s", kept, text)
			}
		}
		member, err := dataTableBusiness.GetBundle(w.ctx, tb.Id, maya)
		if err != nil {
			t.Fatal(err)
		}
		text, _ = json.Marshal(member)
		for _, setting := range []string{"Quiet clause", "Churn watch", vendors.Id.String()} {
			if !strings.Contains(string(text), setting) {
				t.Errorf("the member's bundle lost %q", setting)
			}
		}
	})
}
