//go:build integration
// +build integration

package integration_test

// Links to items stored with their titles, against Postgres 12 with every
// migration (TAB-BUG-5).
//
// Before each read named a row's links to docs, tasks, boards, projects and
// people for its reader, a link was stored with the item's title as it was
// when linked. Reads now name them, but a cell no read names still gave the
// title out as stored: a deleted field's, to members, guests and the AI alike.
// RepairItemLinks reduces every stored link to its id and kind, once per
// install, and leaves every other cell as it was.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableItemLinksRepair -v

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func TestTableItemLinksRepair(t *testing.T) {
	w := newTableWorld(t, "maya")
	maya := w.people["maya"]
	tb := w.table("maya", "Plans")
	docs := w.field("maya", tb, "Docs", dataTableModel.FieldRelation, map[string]interface{}{"relation_target": "doc"})
	tags := w.field("maya", tb, "Tags", dataTableModel.FieldMultiSelect, nil)
	gone := w.field("maya", tb, "Boards", dataTableModel.FieldRelation, map[string]interface{}{"relation_target": "board"})
	plan, layoffs := uuid.NewString(), uuid.NewString()
	// As an install holds them: titles copied into the links (and something
	// else stored beside one), a read's count of links past those shown, and
	// cells that aren't links at all.
	seed := func(cells string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := w.env.PG.QueryRow(`INSERT INTO data_table_rows (id, table_id, values, position, created_by)
			VALUES (uuid_generate_v4(), $1, $2::jsonb, 1, $3) RETURNING id`, tb.Id, cells, maya.UserID).Scan(&id); err != nil {
			t.Fatalf("seed a row: %v", err)
		}
		return id
	}
	linked := seed(fmt.Sprintf(`{%q: [{"id": %q, "label": "Restructuring plan Q4", "type": "doc"}, {"type": "more", "count": 3}],
		%q: ["Q4", "Pune"], %q: {"id": %q, "label": "Layoffs", "type": "board", "color": "red"}, "notes": "plain", "empty": []}`,
		docs, plan, tags, gone, layoffs))
	plain := seed(fmt.Sprintf(`{%q: ["Q4"], "nested": [["a"], {"label": "not a link"}]}`, tags))
	// The Boards field is deleted: no read names its cells any more.
	if err := dataTableBusiness.DeleteField(w.ctx, tb.Id, uuid.MustParse(gone), maya); err != nil {
		t.Fatal(err)
	}
	before := w.stored(tb, plain)

	changed, err := dataTableBusiness.RepairItemLinks(w.ctx)
	if err != nil || changed != 1 {
		t.Fatalf("the repair changed %d rows (%v), want the one with links", changed, err)
	}
	got := w.stored(tb, linked)
	want := map[string]interface{}{
		docs:    []interface{}{map[string]interface{}{"id": plan, "type": "doc"}, map[string]interface{}{"type": "more", "count": float64(3)}},
		tags:    []interface{}{"Q4", "Pune"},
		gone:    map[string]interface{}{"id": layoffs, "type": "board"},
		"notes": "plain",
		"empty": []interface{}{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the linked row holds %v, want %v", got, want)
	}
	if after := w.stored(tb, plain); !reflect.DeepEqual(after, before) {
		t.Errorf("a row with no links changed: %v, was %v", after, before)
	}
	guest, err := dataTableBusiness.GetGuestBundle(w.ctx, tb.Id)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := json.Marshal(guest)
	for _, title := range []string{"Restructuring plan Q4", "Layoffs", "red"} {
		if strings.Contains(string(text), title) {
			t.Errorf("the guest bundle still carries %q", title)
		}
	}

	// Once done, it isn't done again; and done again, it would change nothing.
	if changed, err := dataTableBusiness.RepairItemLinks(w.ctx); err != nil || changed != 0 {
		t.Errorf("a second repair changed %d rows (%v)", changed, err)
	}
	if _, n, err := dataTableModel.StripItemLinks(w.ctx, []string{"board", "doc", "project", "task", "user"}, uuid.Nil, 500); err != nil || n != 0 {
		t.Errorf("stripping again changed %d rows (%v)", n, err)
	}
}
