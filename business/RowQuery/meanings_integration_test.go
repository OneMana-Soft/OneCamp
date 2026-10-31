//go:build integration

package business_test

// The fixture's table through the cell index (tables T5c): each case of
// testdata/rowquery/meanings.json, answered in windows by the SQL, shows the
// rows the evaluator shows (meanings_test.go), which are the rows the
// browser shows itself (the web's meanings.test.ts). A view the server
// windows and one the browser sorts itself mean the same, awkward cell for
// awkward cell: Go's spaces, İ and a final Σ, a choice's places, days and
// moments in the reader's zone, numbers stored as text, blank labels.
//
// Run: integ_rel.sh <worktree> <log> '^(TestTheFixturesTableMeansTheSameThroughTheIndex)$' with PKGS='./business/RowQuery/'

import (
	"slices"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

func TestTheFixturesTableMeansTheSameThroughTheIndex(t *testing.T) {
	m, zone := readMeanings(t)
	// The owner, who asks every case, is the fixture's me: the evaluator's
	// cases are answered for m.Me (meanings_test.go), and @me is the asker.
	ctx, owner := tableOwnerAs(t, m.Me)
	db := postgresInit.DBConn.SqlDB
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Meanings", Visibility: "workspace"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	// Its fields and rows with the fixture's ids, as the API sent them.
	for _, f := range m.Fields {
		if _, err := db.ExecContext(ctx, `INSERT INTO data_table_fields (id, table_id, name, type, config, position) VALUES ($1, $2, $3, $4, $5, $6)`,
			f.Id, tb.Id, f.Name, f.Type, f.Config, f.Position); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range m.Rows {
		if _, err := db.ExecContext(ctx, `INSERT INTO data_table_rows (id, table_id, "values", position, position_key, row_no, created_by, created_at, updated_at)
			VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, $8, $9)`, r.Id, tb.Id, r.Values, r.Position, r.PositionKey, r.RowNo, owner.UserID, r.CreatedAt, r.UpdatedAt); err != nil {
			t.Fatal(err)
		}
	}
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatal(err)
	}

	who := accessBusiness.Subject{UserUUID: owner.UserID.String()}
	at := rowQuery.WithZone(ctx, zone)
	for _, c := range m.Cases {
		// A formula's or a rollup's case is the Computed Store's, which works
		// the values out from the formula again, where the fixture holds them
		// as a read sends them; the 100k test's CONTROL leg holds the store's
		// SQL to a stored copy's order. The index answers every other case.
		if rowQuery.ReadsComputed(c.Spec, m.Fields) {
			continue
		}
		spec := c.Spec
		spec.Scope = query.Scope{Kind: query.ScopeTable, ID: tb.Id.String()}
		var got []string
		var cursor query.Cursor
		for window := 0; ; window++ {
			// Windows of four, so cursors cross ties and blanks.
			res, err := query.Queries.Run(at, who, spec, query.Page{Size: 4, After: cursor})
			if err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
			if window == 0 && res.Next != "" && plan(t, res.Next) == "worked" {
				t.Errorf("%s: answered by the evaluator, not the index", c.Name)
			}
			got = append(got, res.IDs...)
			if res.Next == "" || window > len(m.Rows) {
				break
			}
			cursor = res.Next
		}
		if got == nil {
			got = []string{}
		}
		if !slices.Equal(got, c.IDs) {
			t.Errorf("%s: the index shows\n%v\nthe evaluator\n%v", c.Name, got, c.IDs)
		}
	}
}
