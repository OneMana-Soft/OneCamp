//go:build integration
// +build integration

package integration_test

// A field in the trash keeps its cells for its restore (tables A2), and no
// reader sees them meanwhile. The v1 API's read leaves them out, as every
// read does. A whole row saved by the web released before A2, which never
// shows them, keeps them as stored, even from a copy made before the delete.
// The table's trash of rows leaves them out too. Brought back, the field
// shows them again, everywhere.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAFieldInTheTrashKeepsItsCells -v

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func TestAFieldInTheTrashKeepsItsCells(t *testing.T) {
	w := newTableWorld(t, "amara")
	amara := w.people["amara"]
	deals := w.table("amara", "Deals")
	notes := w.field("amara", deals, "Notes", dataTableModel.FieldText, nil)
	stage := w.field("amara", deals, "Stage", dataTableModel.FieldText, nil)
	row := func(note, st string) uuid.UUID {
		t.Helper()
		r, err := dataTableBusiness.CreateRow(w.ctx, deals.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{notes: note, stage: st}}, amara)
		if err != nil {
			t.Fatalf("make a row: %v", err)
		}
		return r.Id
	}
	acme, globex := row("call back", "Lead"), row("send terms", "Lead")

	// v1Cells is each row's cells as GET /v1/tables/{id} gives them.
	api := w.asMember("amara", http.MethodGet, "/v1/tables/{id}", v1Controller.GetTable)
	v1Cells := func() map[uuid.UUID]map[string]interface{} {
		t.Helper()
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tables/"+deals.Id.String(), nil))
		var out struct {
			Data struct {
				Rows []struct {
					ID     uuid.UUID `json:"id"`
					Values string    `json:"values"`
				} `json:"rows"`
			} `json:"data"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("GET /v1/tables/{id} answered %d: %s", rec.Code, rec.Body.String())
		}
		cells := map[uuid.UUID]map[string]interface{}{}
		for _, r := range out.Data.Rows {
			var v map[string]interface{}
			_ = json.Unmarshal([]byte(r.Values), &v)
			cells[r.ID] = v
		}
		return cells
	}
	// saveWhole saves a row whole, as the web released before A2 does.
	save := w.asMember("amara", http.MethodPost, "/tables/{id}/rows/{rowId}/update", dataTableController.UpdateRow)
	saveWhole := func(r uuid.UUID, values map[string]interface{}) {
		t.Helper()
		body, _ := json.Marshal(map[string]interface{}{"values": values})
		rec := httptest.NewRecorder()
		save.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tables/"+deals.Id.String()+"/rows/"+r.String()+"/update", bytes.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("a whole-row save answered %d: %s", rec.Code, rec.Body.String())
		}
	}
	if got := v1Cells()[acme][notes]; got != "call back" {
		t.Fatalf("before the delete the v1 API reads Acme's note as %v", got)
	}
	// A copy of the row made before the delete, as an open page holds it.
	copied := map[string]interface{}{notes: "call back", stage: "Lead"}

	if err := dataTableBusiness.DeleteField(w.ctx, deals.Id, uuid.MustParse(notes), amara); err != nil {
		t.Fatalf("delete Notes: %v", err)
	}
	for id, c := range v1Cells() {
		if v, in := c[notes]; in {
			t.Errorf("the v1 API reads row %s's cell of the deleted field: %v", id, v)
		}
	}

	// The web released before A2 saves Acme whole, from a copy without the
	// deleted field, and Globex from one made before the delete, changing
	// its note: both keep their notes as stored.
	saveWhole(acme, map[string]interface{}{stage: "Won"})
	copied[stage], copied[notes] = "Won", "changed in an old copy"
	saveWhole(globex, copied)
	if got := w.stored(deals, acme); got[notes] != "call back" || got[stage] != "Won" {
		t.Errorf("after a whole-row save Acme holds %v", got)
	}
	if got := w.stored(deals, globex); got[notes] != "send terms" || got[stage] != "Won" {
		t.Errorf("after a whole-row save from an old copy Globex holds %v", got)
	}
	// Nor does a patch naming it (an agent's, from a field list read before
	// the delete), or a new row: no write reaches a field in the trash.
	if _, err := dataTableBusiness.PatchRow(w.ctx, deals.Id, acme, map[string]interface{}{notes: "patched", stage: "Won"}, amara); err != nil {
		t.Fatalf("patch Acme: %v", err)
	}
	if got := w.stored(deals, acme); got[notes] != "call back" {
		t.Errorf("a patch naming the deleted field left Acme holding %v", got)
	}
	initech := row("never shown", "Lead")
	if got := w.stored(deals, initech); got[notes] != nil {
		t.Errorf("a new row holds a cell of the deleted field: %v", got)
	}

	// A deleted row in the table's trash shows only the live fields' cells.
	if err := dataTableBusiness.DeleteRow(w.ctx, deals.Id, globex, amara); err != nil {
		t.Fatalf("delete Globex: %v", err)
	}
	trashed := func() map[string]interface{} {
		t.Helper()
		tr, err := dataTableBusiness.TableTrash(w.ctx, deals.Id, nil, amara)
		if err != nil {
			t.Fatalf("read the trash: %v", err)
		}
		for _, r := range tr.Rows {
			if r.ID == globex {
				var v map[string]interface{}
				_ = json.Unmarshal(r.Values, &v)
				return v
			}
		}
		t.Fatal("Globex isn't in the trash")
		return nil
	}
	if got := trashed(); got[stage] != "Won" || got[notes] != nil {
		t.Errorf("the trash shows Globex as %v", got)
	}

	// Brought back, the field shows its cells again: in the v1 read and the trash.
	if _, err := dataTableBusiness.RestoreFromTrash(w.ctx, deals.Id, dataTableBusiness.RestoreInput{Fields: []uuid.UUID{uuid.MustParse(notes)}}, amara); err != nil {
		t.Fatalf("restore Notes: %v", err)
	}
	if got := v1Cells()[acme]; got[notes] != "call back" || got[stage] != "Won" {
		t.Errorf("after the restore the v1 API reads Acme as %v", got)
	}
	if got := trashed(); got[notes] != "send terms" {
		t.Errorf("after the restore the trash shows Globex as %v", got)
	}
}
