//go:build integration
// +build integration

package integration_test

// A table's header changed from two tabs, against Postgres 12 with every
// migration (TAB-BUG-12).
//
// The header was saved whole, from each page's copy of the table. Maya made
// "Restructuring" private in one tab; her other tab, opened before, still said
// "workspace", and a rename there made the table visible to the whole
// workspace again, with nothing to say so. Now POST /tables/{id}/update
// changes only the fields it names, tells open pages, and a change of who can
// see a table goes in the audit log.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableHeaderChanges -v

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func TestTableHeaderChanges(t *testing.T) {
	w := newTableWorld(t, "maya", "bo")
	tb := w.table("maya", "Restructuring")
	broker := w.listen()
	h := w.asMember("maya", http.MethodPost, "/tables/{id}/update", dataTableController.UpdateTable)
	// tab sends a header change as one of Maya's tabs does.
	tab := func(t *testing.T, body string) (int, *dataTableModel.DataTable) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tables/"+tb.Id.String()+"/update", bytes.NewBufferString(body)))
		var out struct {
			Data *dataTableModel.DataTable `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Data
	}
	boSees := func(t *testing.T) bool {
		t.Helper()
		list, err := dataTableBusiness.ListTables(w.ctx, w.people["bo"])
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range list {
			if x.Id == tb.Id {
				return true
			}
		}
		return false
	}
	if !boSees(t) {
		t.Fatal("Bo can't see the workspace table to begin with")
	}

	// Both tabs have the table as "workspace". The first makes it private.
	if code, got := tab(t, `{"visibility":"private"}`); code != http.StatusOK || got.Visibility != "private" || got.Name != "Restructuring" {
		t.Fatalf("making it private answered %d %+v", code, got)
	}
	// The second, still holding "workspace", renames it and adds a description.
	if code, got := tab(t, `{"name":"Restructuring Q4"}`); code != http.StatusOK || got.Visibility != "private" || got.Name != "Restructuring Q4" {
		t.Errorf("the rename answered %d %+v", code, got)
	}
	if code, got := tab(t, `{"description":"Draft"}`); code != http.StatusOK || got.Visibility != "private" || got.Name != "Restructuring Q4" ||
		got.Description == nil || *got.Description != "Draft" {
		t.Errorf("the description answered %d %+v", code, got)
	}
	if boSees(t) {
		t.Error("a rename from a stale tab showed the private table to Bo")
	}
	if code, got := tab(t, `{"description":""}`); code != http.StatusOK || got.Description != nil {
		t.Errorf("clearing the description answered %d %+v", code, got)
	}
	if code, _ := tab(t, `{}`); code != http.StatusBadRequest {
		t.Errorf("a change naming nothing answered %d, want 400", code)
	}
	if code, _ := tab(t, `{"visibility":"everyone"}`); code != http.StatusBadRequest {
		t.Errorf("a visibility there is no such thing as answered %d, want 400", code)
	}
	// A page still sending the whole header with each rename puts back the
	// visibility its copy holds: it's refused, loudly, and changes nothing.
	if code, _ := tab(t, `{"name":"Restructuring","description":"","visibility":"workspace"}`); code != http.StatusBadRequest {
		t.Errorf("the whole header answered %d, want 400", code)
	}
	if got, err := dataTableModel.GetTableByID(w.ctx, tb.Id); err != nil || got.Name != "Restructuring Q4" || got.Visibility != "private" || boSees(t) {
		t.Errorf("after the whole header was refused the table is %+v (%v)", got, err)
	}

	t.Run("open pages hear of each change", func(t *testing.T) {
		n := 0
		for _, a := range tableActions(broker, tb) {
			if a == "table" {
				n++
			}
		}
		if n != 4 {
			t.Errorf("the table's topic was told of %d header changes, want 4", n)
		}
	})

	t.Run("making it private is in the audit log, and nothing else is", func(t *testing.T) {
		var n int
		for i := 0; i < 50; i++ { // the audit entry is written off the request
			if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE action = 'table.visibility' AND metadata::jsonb ->> 'table_id' = $1`,
				tb.Id.String()).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n > 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if n != 1 {
			t.Errorf("%d visibility changes recorded, want 1", n)
		}
	})
}

// The web released before patches (v2.72.2's) sends its copy of the header
// whole with each save: the name, the description and icon when it has them,
// and who can see the table, from app/app/tables/[table-id]/page.tsx's
// saveTable (a rename, a description, Make private, Show to the workspace)
// and the tables list's rename (app/app/tables/page.tsx). Open tabs keep
// that web through a deploy, and a server updated before its web is reached
// by it for as long as that takes, so each of its saves does what it did,
// and a copy read before the table changed is refused, not written.
func TestTableHeaderFromTheReleasedWeb(t *testing.T) {
	w := newTableWorld(t, "maya", "bo")
	tb := w.table("maya", "Plan")
	h := w.asMember("maya", http.MethodPost, "/tables/{id}/update", dataTableController.UpdateTable)
	// save posts body as that web does, and answers the code and the table
	// as it's stored; said is what a refusal said.
	var said string
	save := func(t *testing.T, body string) (int, *dataTableModel.DataTable) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tables/"+tb.Id.String()+"/update", bytes.NewBufferString(body)))
		var out struct {
			Msg string `json:"msg"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		said = out.Msg
		got, err := dataTableModel.GetTableByID(w.ctx, tb.Id)
		if err != nil || got == nil {
			t.Fatalf("read the table: %v", err)
		}
		return rec.Code, got
	}
	description := func(got *dataTableModel.DataTable) string {
		if got.Description == nil {
			return ""
		}
		return *got.Description
	}
	boSees := func(t *testing.T) bool {
		t.Helper()
		list, err := dataTableBusiness.ListTables(w.ctx, w.people["bo"])
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range list {
			if x.Id == tb.Id {
				return true
			}
		}
		return false
	}
	if code, got := save(t, `{"name":"Plan","description":"Draft","visibility":"workspace"}`); code != http.StatusOK || description(got) != "Draft" {
		t.Fatalf("adding a description answered %d, and the table is %+v", code, got)
	}
	if code, got := save(t, `{"name":"Plan Q4","description":"Draft","visibility":"workspace"}`); code != http.StatusOK ||
		got.Name != "Plan Q4" || description(got) != "Draft" || got.Visibility != "workspace" {
		t.Errorf("a rename answered %d, and the table is %q %q %s", code, got.Name, description(got), got.Visibility)
	}
	if code, got := save(t, `{"name":"Plan Q4","description":"Numbers for the board","visibility":"workspace"}`); code != http.StatusOK ||
		got.Name != "Plan Q4" || description(got) != "Numbers for the board" {
		t.Errorf("a description answered %d, and the table is %q %q", code, got.Name, description(got))
	}
	// A cleared description is left out of the body.
	if code, got := save(t, `{"name":"Plan Q4","visibility":"workspace"}`); code != http.StatusOK || got.Description != nil {
		t.Errorf("clearing the description answered %d, and it is %q", code, description(got))
	}
	if code, got := save(t, `{"name":"Plan Q4","visibility":"private"}`); code != http.StatusOK || got.Visibility != "private" || boSees(t) {
		t.Errorf("Make private answered %d, and the table is %s (Bo sees it: %v)", code, got.Visibility, boSees(t))
	}
	// The list, still holding the table as it was before Make private,
	// renames it: its copy would show the table to the workspace again. The
	// refusal says why, and what to do.
	if code, got := save(t, `{"name":"Plan Q4 final","visibility":"workspace"}`); code != http.StatusBadRequest ||
		got.Name != "Plan Q4" || got.Visibility != "private" || boSees(t) {
		t.Errorf("a rename from a stale copy answered %d, and the table is %q %s (Bo sees it: %v)", code, got.Name, got.Visibility, boSees(t))
	}
	if said != "this table changed since the page was opened; reload it and try again" {
		t.Errorf("the refusal said %q", said)
	}
	if code, got := save(t, `{"name":"Plan Q4","visibility":"workspace"}`); code != http.StatusOK || got.Visibility != "workspace" || !boSees(t) {
		t.Errorf("Show to the workspace answered %d, and the table is %s", code, got.Visibility)
	}

	t.Run("each change of who can see it is in the audit log", func(t *testing.T) {
		recorded := func() int {
			var n int
			if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE action = 'table.visibility' AND metadata::jsonb ->> 'table_id' = $1`,
				tb.Id.String()).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		// The audit entry is written off the request.
		waitFor(t, "two visibility changes in the audit log", func() bool { return recorded() >= 2 })
		if n := recorded(); n != 2 {
			t.Errorf("%d visibility changes recorded, want 2", n)
		}
	})
}
