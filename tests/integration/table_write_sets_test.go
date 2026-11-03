//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	rowLedgerController "github.com/akashc777/OneCamp/controllers/RowLedger"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// TestRowWritesAnswerTheirChangeSet: every row write the app makes (a row
// added on the grid, the board or the calendar; a cell patched, a card or a
// bar moved; a row written whole; a link) answers the change set it stored,
// so an Undo pressed while writes are on their way names THAT write (V-360,
// CR-190 MUST 1). The server's own "latest" is the change that landed last:
// two edits made A then B that land B then A would undo A.
func TestRowWritesAnswerTheirChangeSet(t *testing.T) {
	w := newTableWorld(t, "amara")
	vendors := w.table("amara", "Vendors")
	vName := w.field("amara", vendors, "Vendor", dataTableModel.FieldText, nil)
	budget := w.table("amara", "Budget")
	item := w.field("amara", budget, "Item", dataTableModel.FieldText, nil)
	vendor := w.field("amara", budget, "Vendor", dataTableModel.FieldRelation,
		map[string]interface{}{"relation_target": "table", "table_id": vendors.Id.String()})
	acme, err := dataTableBusiness.CreateRow(w.ctx, vendors.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{vName: "Acme"}}, w.people["amara"])
	if err != nil {
		t.Fatal(err)
	}
	base := "/tables/" + budget.Id.String()

	// send serves one request as Amara through h, on the route the router gives it.
	send := func(t *testing.T, method, route, path, body string, h http.HandlerFunc) (int, map[string]json.RawMessage) {
		t.Helper()
		rec := httptest.NewRecorder()
		w.asMember("amara", method, route, h).ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
		var out struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s answered %d: %s", method, path, rec.Code, rec.Body.String())
		}
		return rec.Code, out.Data
	}
	// stored is the change set an answer names, which must be one the table stored.
	stored := func(t *testing.T, what string, code, want int, data map[string]json.RawMessage) uuid.UUID {
		t.Helper()
		if code != want {
			t.Fatalf("%s answered %d, want %d: %v", what, code, want, data)
		}
		raw, ok := data["change_set"]
		if !ok {
			t.Fatalf("%s's answer names no change_set, so an Undo pressed on its way can only take whichever change landed last", what)
		}
		var id uuid.UUID
		if err := json.Unmarshal(raw, &id); err != nil || id == uuid.Nil {
			t.Fatalf("%s's change_set is %s, want the id of the change set it stored", what, raw)
		}
		var n int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM data_table_change_sets WHERE id = $1 AND table_id = $2`, id, budget.Id).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s's change_set %s isn't a change set the table stored (%d, %v)", what, id, n, err)
		}
		return id
	}
	// nothing is an answer whose write changed nothing: it names no change set, and says so.
	nothing := func(t *testing.T, what string, code int, data map[string]json.RawMessage) {
		t.Helper()
		if raw, ok := data["change_set"]; code != http.StatusOK || !ok || string(raw) != "null" {
			t.Errorf("%s answered %d with change_set %s (named: %v), want 200 and null: it stored nothing to undo", what, code, raw, ok)
		}
	}
	idOf := func(t *testing.T, data map[string]json.RawMessage) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := json.Unmarshal(data["id"], &id); err != nil {
			t.Fatalf("the answer names no row: %v", data)
		}
		return id
	}
	add := func(t *testing.T, value string) (int, map[string]json.RawMessage) {
		return send(t, http.MethodPost, "/tables/{id}/rows", base+"/rows", `{"values":{"`+item+`":"`+value+`"}}`, dataTableController.CreateRow)
	}
	patch := func(t *testing.T, row uuid.UUID, value string) (int, map[string]json.RawMessage) {
		return send(t, http.MethodPost, "/tables/{id}/rows/{rowId}/patch", base+"/rows/"+row.String()+"/patch",
			`{"values":{"`+item+`":"`+value+`"}}`, dataTableController.PatchRow)
	}
	link := func(t *testing.T, row uuid.UUID) (int, map[string]json.RawMessage) {
		return send(t, http.MethodPost, "/tables/{id}/rows/{rowId}/links", base+"/rows/"+row.String()+"/links",
			`{"field":"`+vendor+`","add":["`+acme.Id.String()+`"]}`, dataTableController.ChangeLinks)
	}

	// made is a row of the budget, made as the app's own code makes one: each
	// subtest starts from rows of its own.
	made := func(t *testing.T, value string) uuid.UUID {
		t.Helper()
		r, err := dataTableBusiness.CreateRow(w.ctx, budget.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{item: value}}, w.people["amara"])
		if err != nil {
			t.Fatal(err)
		}
		return r.Id
	}

	t.Run("a row added, a cell patched, a row written whole and a link each answer the change set they stored", func(t *testing.T) {
		booth, banner := made(t, "Booth"), made(t, "Banner")
		code, data := add(t, "Badges")
		stored(t, "a row added", code, http.StatusCreated, data)
		if idOf(t, data) == uuid.Nil {
			t.Error("the row added has no id")
		}
		code, data = patch(t, booth, "Big booth")
		stored(t, "a cell's patch", code, http.StatusOK, data)
		code, data = send(t, http.MethodPut, "/tables/{id}/rows/{rowId}", base+"/rows/"+banner.String(),
			`{"values":{"`+item+`":"Big banner"}}`, dataTableController.UpdateRow)
		stored(t, "a row written whole", code, http.StatusOK, data)
		code, data = link(t, booth)
		stored(t, "a link", code, http.StatusOK, data)
	})

	t.Run("an undo naming a write's change set takes back that write, not the one that landed after it", func(t *testing.T) {
		stage, signs := made(t, "Stage"), made(t, "Signs")
		// The signs' edit was made last but landed first, and the stage's
		// landed after it. An Undo pressed while both were on their way takes
		// the signs' edit, by its set; with no set to name, the server's
		// latest is the stage's.
		code, data := patch(t, signs, "Signs, made last")
		setSigns := stored(t, "the signs' patch", code, http.StatusOK, data)
		code, data = patch(t, stage, "Stage, landed last")
		stored(t, "the stage's patch", code, http.StatusOK, data)
		code, data = send(t, http.MethodPost, "/tables/{id}/undo", base+"/undo", `{"change_set":"`+setSigns.String()+`"}`, rowLedgerController.Undo)
		var reversed uuid.UUID
		if err := json.Unmarshal(data["reversed"], &reversed); code != http.StatusOK || err != nil || reversed != setSigns {
			t.Fatalf("the undo naming the signs' set answered %d, reversing %s; want 200 and %s: %v", code, data["reversed"], setSigns, data)
		}
		if got := w.stored(budget, signs)[item]; got != "Signs" {
			t.Errorf("the signs read %v after their edit's undo, want Signs", got)
		}
		if got := w.stored(budget, stage)[item]; got != "Stage, landed last" {
			t.Errorf("the stage reads %v after the signs' undo, want its own edit, Stage, landed last", got)
		}
	})

	t.Run("a write that changed nothing answers change_set null: it stored nothing to undo", func(t *testing.T) {
		chairs := made(t, "Chairs")
		if _, _, err := dataTableBusiness.ChangeLinks(w.ctx, budget.Id, chairs, uuid.MustParse(vendor), []uuid.UUID{acme.Id}, nil, w.people["amara"]); err != nil {
			t.Fatal(err)
		}
		code, data := patch(t, chairs, "Chairs")
		nothing(t, "a patch to the value the cell holds", code, data)
		code, data = link(t, chairs)
		nothing(t, "a link there already", code, data)
		// A batch (a paste, a fill) that sets a cell to the value it holds: the web offers no toast Undo for a
		// null set, since an Undo naming none would take the reader's latest change instead.
		code, data = send(t, http.MethodPost, "/tables/{id}/rows/batch", base+"/rows/batch",
			`{"patch":[{"row":"`+chairs.String()+`","values":{"`+item+`":"Chairs"}}]}`, rowLedgerController.Batch)
		nothing(t, "a batch setting a cell to the value it holds", code, data)
	})
}
