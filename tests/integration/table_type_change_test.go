//go:build integration
// +build integration

package integration_test

// A column's type changed on a table past the page the web holds, against
// Postgres 12 with every migration (TAB-BUG-9).
//
// Changing a column's type stored the new type and left every cell as it was,
// though the menu promised to clear what didn't fit: text turned into
// checkboxes showed every filled cell ticked. Now the cells are converted in
// the field update's transaction, over every row, and a dry run counts it
// first, the same way.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableTypeChange -v

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	"github.com/akashc777/OneCamp/business/Formula"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func TestTableTypeChange(t *testing.T) {
	w := newTableWorld(t, "owner")
	me := w.people["owner"]
	tb := w.table("owner", "Orders")
	amount := w.field("owner", tb, "Amount", dataTableModel.FieldText, nil)
	stage := w.field("owner", tb, "Stage", dataTableModel.FieldSelect,
		map[string]interface{}{"options": []interface{}{map[string]interface{}{"label": "Open"}, map[string]interface{}{"label": "Won"}}})
	kind := w.field("owner", tb, "Kind", dataTableModel.FieldText, nil)
	// 1,200 rows: amounts as numbers in words, numbers, ticks and nothing;
	// stages including one no option offers; kinds as free text.
	if _, err := w.env.PG.Exec(`INSERT INTO data_table_rows (id, table_id, values, position, created_by)
		SELECT uuid_generate_v4(), $1, jsonb_strip_nulls(jsonb_build_object(
			$2::text, (ARRAY['1,200', 'yes', 'Done', NULL])[n % 4 + 1],
			$3::text, (ARRAY['Open', 'Won', 'Lost'])[n % 3 + 1],
			$4::text, (ARRAY['Retail', 'Wholesale'])[n % 2 + 1])), n, $5
		FROM generate_series(0, 1199) n`, tb.Id, amount, stage, kind, me.UserID); err != nil {
		t.Fatal(err)
	}
	preview := func(t *testing.T, field, to string) dataTableBusiness.ConvertCounts {
		t.Helper()
		c, err := dataTableBusiness.PreviewConversion(w.ctx, tb.Id, uuidOf(t, field), to, formula.NumberSigns{}, me)
		if err != nil {
			t.Fatal(err)
		}
		return *c
	}
	change := func(t *testing.T, field, name, to string, cfg map[string]interface{}) int {
		t.Helper()
		n, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, uuidOf(t, field), dataTableBusiness.FieldPatch{Name: &name, Type: &to, Config: cfg}, me)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	count := func(t *testing.T, where string, args ...interface{}) int {
		t.Helper()
		var n int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM data_table_rows WHERE table_id = $1 AND `+where,
			append([]interface{}{tb.Id}, args...)...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("text to checkbox and to number: the dry run counts what the change does", func(t *testing.T) {
		if got := preview(t, amount, dataTableModel.FieldCheckbox); got != (dataTableBusiness.ConvertCounts{Converted: 300, Cleared: 600}) {
			t.Errorf("to checkbox would %+v", got)
		}
		want := dataTableBusiness.ConvertCounts{Converted: 300, Cleared: 600}
		if got := preview(t, amount, dataTableModel.FieldNumber); got != want {
			t.Errorf("to number would %+v, want %+v", got, want)
		}
		if n := change(t, amount, "Amount", dataTableModel.FieldNumber, nil); n != 900 {
			t.Errorf("changed %d rows, want 900", n)
		}
		if a, b := count(t, `values -> $2 = '1200'::jsonb`, amount), count(t, `values -> $2 = 'null'::jsonb`, amount); a != 300 || b != 600 {
			t.Errorf("after the change %d rows hold 1200 and %d are cleared", a, b)
		}
		if got := preview(t, amount, dataTableModel.FieldNumber); got != (dataTableBusiness.ConvertCounts{}) {
			t.Errorf("a field already a number would %+v", got)
		}
	})

	t.Run("select to multi-select and back keeps every value", func(t *testing.T) {
		if n := change(t, stage, "Stage", dataTableModel.FieldMultiSelect, nil); n != 1200 {
			t.Errorf("to multi-select changed %d rows", n)
		}
		if n := count(t, `values -> $2 = '["Lost"]'::jsonb`, stage); n != 400 {
			t.Errorf("%d rows hold [Lost]", n)
		}
		if n := change(t, stage, "Stage", dataTableModel.FieldSelect, nil); n != 1200 || count(t, `values ->> $2 = 'Won'`, stage) != 400 {
			t.Errorf("back to select changed %d rows", n)
		}
	})

	t.Run("text to select keeps each label, and offers those it didn't", func(t *testing.T) {
		if got := preview(t, kind, dataTableModel.FieldSelect); got != (dataTableBusiness.ConvertCounts{Kept: 1200}) {
			t.Errorf("to select would %+v", got)
		}
		if n := change(t, kind, "Kind", dataTableModel.FieldSelect, map[string]interface{}{"options": []interface{}{map[string]interface{}{"label": "Retail"}}}); n != 0 {
			t.Errorf("to select changed %d rows", n)
		}
		var cfg string
		if err := w.env.PG.QueryRow(`SELECT config::text FROM data_table_fields WHERE id = $1`, kind).Scan(&cfg); err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Options []struct{ ID, Label string } `json:"options"`
		}
		_ = json.Unmarshal([]byte(cfg), &parsed)
		var labels []string
		for _, o := range parsed.Options {
			if o.ID == "" {
				t.Errorf("option %q has no id", o.Label)
			}
			labels = append(labels, o.Label)
		}
		if strings.Join(labels, ",") != "Retail,Wholesale" {
			t.Errorf("the field offers %v", labels)
		}
	})

	// Text becomes numbers as whoever changes the column writes them (the
	// tables audit's B3): "1,5" was read by dropping its comma, 15 for good,
	// and one "NaN" failed the whole change. What reads two ways is cleared,
	// and the dry run counts it, so the menu says so first.
	t.Run("a column made numbers reads its text in the signs of whoever changes it", func(t *testing.T) {
		rate := w.field("owner", tb, "Rate", dataTableModel.FieldText, nil)
		if _, err := w.env.PG.Exec(`UPDATE data_table_rows SET values = values || jsonb_build_object($2::text, (ARRAY['1,5','1.234,56','NaN','Infinity'])[position::int % 4 + 1])
			WHERE table_id = $1`, tb.Id, rate); err != nil {
			t.Fatal(err)
		}
		if got := preview(t, rate, dataTableModel.FieldNumber); got != (dataTableBusiness.ConvertCounts{Cleared: 1200}) {
			t.Errorf("in en-US's signs the change would %+v, want every value cleared", got)
		}
		send := func(t *testing.T, route string, h http.HandlerFunc, body string) (int, map[string]interface{}) {
			t.Helper()
			rec := httptest.NewRecorder()
			w.asMember("owner", http.MethodPost, "/tables/{id}/fields/{fieldId}/"+route, h).
				ServeHTTP(rec, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/tables/%s/fields/%s/%s", tb.Id, rate, route), bytes.NewBufferString(body)))
			var out map[string]interface{}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			return rec.Code, out
		}
		code, out := send(t, "convert-preview", dataTableController.PreviewConversion, `{"type":"number","decimal":",","group":"."}`)
		if data, _ := out["data"].(map[string]interface{}); code != http.StatusOK || data["converted"] != float64(600) || data["cleared"] != float64(600) {
			t.Errorf("in de-DE's signs the dry run answered %d %v, want 600 converted and 600 cleared", code, out)
		}
		if code, out := send(t, "convert-preview", dataTableController.PreviewConversion, `{"type":"number","decimal":"1"}`); code != http.StatusBadRequest {
			t.Errorf("signs no number is written with answered %d %v", code, out)
		}
		code, out = send(t, "update", dataTableController.UpdateField, `{"name":"Rate","type":"number","decimal":",","group":"."}`)
		if code != http.StatusOK || out["cells_changed"] != float64(1200) {
			t.Fatalf("the change answered %d %v", code, out)
		}
		for want, n := range map[string]int{"1.5": 300, "1234.56": 300, "null": 600} {
			if got := count(t, `values -> $2 = $3::jsonb`, rate, want); got != n {
				t.Errorf("%d rows hold %s, want %d", got, want, n)
			}
		}
	})
}
