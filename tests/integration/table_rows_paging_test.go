//go:build integration
// +build integration

package integration_test

// A table past one page of rows, against Postgres 12 with every migration
// (TAB-BUG-2).
//
// The table page held the bundle's first 500 rows and nothing else. A new
// row was put after the last row the page held, which on a 600-row table
// sorted it after the 500th, outside the page: Add row made a row that never
// appeared. Now a row made without a position goes after the table's last row,
// the bundle says how many rows the table has, and pages are read in one
// order, ties and all, so a reader paging through meets every row once.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableRowsPastOnePage -v

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func TestTableRowsPastOnePage(t *testing.T) {
	w := newTableWorld(t, "owner")
	me := w.people["owner"]
	tb := w.table("owner", "Leads")
	name := w.field("owner", tb, "Company", dataTableModel.FieldText, nil)
	// 600 rows at places 0 to 599, as an import or a paste makes them.
	// (Postgres 12 has no gen_random_uuid; uuid-ossp's uuid_generate_v4 is
	// installed by the migrations.)
	if _, err := w.env.PG.Exec(`INSERT INTO data_table_rows (id, table_id, values, position, created_by)
		SELECT uuid_generate_v4(), $1, jsonb_build_object($2::text, 'Company ' || n), n, $3
		FROM generate_series(0, 599) n`, tb.Id, name, me.UserID); err != nil {
		t.Fatalf("seed 600 rows: %v", err)
	}

	bundle, err := dataTableBusiness.GetBundle(w.ctx, tb.Id, me)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Rows) != 500 || !bundle.RowsTruncated || bundle.RowCount != 600 {
		t.Fatalf("the bundle carries %d rows (truncated %v) of %d, want 500 of 600", len(bundle.Rows), bundle.RowsTruncated, bundle.RowCount)
	}

	t.Run("a new row without a place goes last, and the next page has it", func(t *testing.T) {
		created, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{name: "Newco"}}, me)
		if err != nil {
			t.Fatal(err)
		}
		if created.Position != 600 {
			t.Errorf("the new row is at %v, want 600, after the last row", created.Position)
		}
		page, err := dataTableBusiness.ListRows(w.ctx, tb.Id, me, 500, 500)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 101 || page[len(page)-1].Id != created.Id {
			t.Errorf("the page after the bundle has %d rows, ending %v; want 101 ending with the new row", len(page), page[len(page)-1].Id)
		}
		if b, _ := dataTableBusiness.GetBundle(w.ctx, tb.Id, me); b == nil || b.RowCount != 601 {
			t.Errorf("after adding a row the bundle counts %v rows, want 601", b)
		}
	})

	t.Run("a place given is kept, and an update without one keeps the row's", func(t *testing.T) {
		first := -1.0
		created, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{name: "Aardvark"}, Position: &first}, me)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dataTableBusiness.UpdateRow(w.ctx, tb.Id, created.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{name: "Aardvark Ltd"}}, me); err != nil {
			t.Fatal(err)
		}
		page, err := dataTableBusiness.ListRows(w.ctx, tb.Id, me, 1, 0)
		if err != nil || len(page) != 1 || page[0].Id != created.Id || page[0].Position != -1 {
			t.Errorf("the row put first and then updated isn't first at -1: %v %v", page, err)
		}
	})

	t.Run("deleted rows aren't counted", func(t *testing.T) {
		page, err := dataTableBusiness.ListRows(w.ctx, tb.Id, me, 2, 0)
		if err != nil || len(page) != 2 {
			t.Fatal(err)
		}
		before, _ := dataTableBusiness.GetBundle(w.ctx, tb.Id, me)
		for _, r := range page {
			if err := dataTableBusiness.DeleteRow(w.ctx, tb.Id, r.Id, me); err != nil {
				t.Fatal(err)
			}
		}
		after, err := dataTableBusiness.GetBundle(w.ctx, tb.Id, me)
		if err != nil || after.RowCount != before.RowCount-2 {
			t.Errorf("after deleting two rows the bundle counts %d, want %d", after.RowCount, before.RowCount-2)
		}
		guest, err := dataTableBusiness.GetGuestBundle(w.ctx, tb.Id)
		if err != nil || guest.RowCount != after.RowCount {
			t.Errorf("the guest bundle counts %v rows, want %d (%v)", guest, after.RowCount, err)
		}
	})

	t.Run("rows that tie on place and age are paged in one order, each once", func(t *testing.T) {
		ties := w.table("owner", "Ties")
		label := w.field("owner", ties, "Name", dataTableModel.FieldText, nil)
		// One statement: the same position and the same created_at for all.
		if _, err := w.env.PG.Exec(`INSERT INTO data_table_rows (id, table_id, values, position, created_by)
			SELECT uuid_generate_v4(), $1, jsonb_build_object($2::text, 'Tie ' || n), 7, $3
			FROM generate_series(1, 40) n`, ties.Id, label, me.UserID); err != nil {
			t.Fatal(err)
		}
		seen := map[uuid.UUID]bool{}
		var order []uuid.UUID
		for offset := 0; offset < 40; offset += 3 {
			page, err := dataTableBusiness.ListRows(w.ctx, ties.Id, me, 3, offset)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range page {
				if seen[r.Id] {
					t.Errorf("row %s came twice", r.Id)
				}
				seen[r.Id] = true
				order = append(order, r.Id)
			}
		}
		if len(seen) != 40 {
			t.Errorf("paging three at a time met %d of the 40 tied rows", len(seen))
		}
		for i := 1; i < len(order); i++ {
			if order[i-1].String() >= order[i].String() {
				t.Errorf("tied rows aren't in id order at %d: %s then %s", i, order[i-1], order[i])
				break
			}
		}
		// The link picker reads them the same way: its ids a page at a time,
		// and its search on the label.
		var picked []uuid.UUID
		for offset := 0; offset < 40; offset += 3 {
			page, err := dataTableModel.ListRowIDs(w.ctx, ties.Id, 3, offset)
			if err != nil {
				t.Fatal(err)
			}
			picked = append(picked, page...)
		}
		found, err := dataTableModel.SearchRowsByCell(w.ctx, ties.Id, label, "Tie", 40)
		if err != nil {
			t.Fatal(err)
		}
		for i := range order {
			if i >= len(picked) || picked[i] != order[i] || i >= len(found) || found[i].Id != order[i] {
				t.Errorf("the link picker's reads stray from the table's order at %d", i)
				break
			}
		}
	})
}

// A page of rows the database can't give just now, or whose fields it can't
// give to work the rows out for their reader, answers 503 for the app to
// offer Try again. It answered 400, which the app reads as a request it got
// wrong, the second time with the database's own words. Another transaction
// holding each store makes its read wait out the server's timeout.
func TestTableRowsUnavailable(t *testing.T) {
	w := newTableWorld(t, "owner")
	tb := w.table("owner", "Leads")
	if _, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{}, w.people["owner"]); err != nil {
		t.Fatal(err)
	}
	h := w.asMember("owner", http.MethodGet, "/tables/{id}/rows", dataTableController.ListRows)
	for _, store := range []string{"data_table_fields", "data_table_rows"} {
		t.Run(store+" can't be read", func(t *testing.T) {
			tx, err := w.env.PG.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`LOCK TABLE ` + store + ` IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tables/"+tb.Id.String()+"/rows", nil))
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Couldn't load the rows.") {
				t.Errorf("the rows answered %d %s, want 503 and that they couldn't be loaded", rec.Code, rec.Body)
			}
		})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tables/"+tb.Id.String()+"/rows", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("once the stores are free the rows answered %d %s", rec.Code, rec.Body)
	}
}
