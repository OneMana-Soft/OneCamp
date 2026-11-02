package business

import (
	"slices"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// One pass shares its budget of row pages among the claimed tables, fewest
// pages first and claim order between equals: each table whose pages fit
// goes for good, and the first that doesn't takes what is left and keeps its
// claim, with the tables after it (V-121 S1). The integration leg is
// TestARetentionPassFinishesTheTablesWhosePagesFit.
func TestAPassTakesTheTablesWhosePagesFitFewestFirst(t *testing.T) {
	var held []model.Holding
	pagesOf := map[uuid.UUID][]uuid.UUID{}
	rowsOf := map[uuid.UUID][]string{}
	table := func(pages int) uuid.UUID {
		id := uuid.New()
		for i := 0; i <= pages; i++ {
			h := model.Holding{Table: id, Row: uuid.New()} // the first row has no page
			if i > 0 {
				h.Page = uuid.New()
				pagesOf[id] = append(pagesOf[id], h.Page)
			}
			held = append(held, h)
			rowsOf[id] = append(rowsOf[id], h.Row.String())
		}
		return id
	}
	big, small, mid, twin := table(3), table(1), table(2), table(1)
	bare := uuid.New() // no rows at all
	claimed := []uuid.UUID{big, small, bare, mid}
	names := map[uuid.UUID]string{big: "big", small: "small", mid: "mid", twin: "twin", bare: "bare", uuid.Nil: "none"}
	for _, c := range []struct {
		name    string
		claimed []uuid.UUID
		budget  int
		done    []uuid.UUID
		cut     uuid.UUID
		taken   int // of the cut table's pages
	}{
		{"every table fits", claimed, 6, []uuid.UUID{bare, small, mid, big}, uuid.Nil, 0},
		{"the big table is cut short with what is left", claimed, 4, []uuid.UUID{bare, small, mid}, big, 1},
		{"an exact fit leaves the cut table no pages", claimed, 3, []uuid.UUID{bare, small, mid}, big, 0},
		{"a table with no pages fits any budget", claimed, 0, []uuid.UUID{bare}, small, 0},
		{"claim order between equals", []uuid.UUID{twin, small}, 1, []uuid.UUID{twin}, small, 0},
		{"one table bigger than the pass", []uuid.UUID{big}, 2, nil, big, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := planPass(c.claimed, held, c.budget)
			var gotDone, wantDone []string
			for _, id := range p.done {
				gotDone = append(gotDone, names[id])
			}
			for _, id := range c.done {
				wantDone = append(wantDone, names[id])
			}
			if !slices.Equal(gotDone, wantDone) || p.cut != c.cut {
				t.Fatalf("done %v, cut %s; want done %v, cut %s", gotDone, names[p.cut], wantDone, names[c.cut])
			}
			var rows []string
			var pages []uuid.UUID
			for _, id := range c.done {
				rows = append(rows, rowsOf[id]...)
				pages = append(pages, pagesOf[id]...)
			}
			if c.cut != uuid.Nil {
				pages = append(pages, pagesOf[c.cut][:c.taken]...)
			}
			if !slices.Equal(p.rows, rows) {
				t.Errorf("rows %s; want the done tables' rows %s", strings.Join(p.rows, ","), strings.Join(rows, ","))
			}
			if !slices.Equal(p.pages, pages) || len(p.pages) > c.budget {
				t.Errorf("pages %v; want the done tables' pages, then the cut table's first %d, within %d", p.pages, c.taken, c.budget)
			}
		})
	}
}
