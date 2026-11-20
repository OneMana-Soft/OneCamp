//go:build integration

package business

import (
	"context"
	"strings"
	"testing"
	"time"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// noGraph is an address no Dgraph listens on: tables make no graph call, and
// one would show in Graph().
const noGraph = "127.0.0.1:1"

// settled waits for the work a call left running (its live message, its
// autofill, its webhooks) to stop sending statements, so they are counted
// as the call's.
func settled(calls *integration.StoreCalls) {
	last, still := -1, time.Time{}
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if n := calls.Statements(); n != last {
			last, still = n, time.Now()
		} else if time.Since(still) > 750*time.Millisecond {
			return
		}
	}
}

// said logs what a call sent, a line a statement, for its red and green logs.
func said(t *testing.T, what string, calls *integration.StoreCalls) {
	t.Helper()
	t.Logf("%s: %d trips, %d statements, %d batches, %d graph", what, calls.Trips(), calls.Statements(), calls.Batches(), calls.Graph())
	for i, s := range calls.Said() {
		if i == 60 {
			t.Logf("  ... %d more", len(calls.Said())-60)
			break
		}
		t.Logf("  %s", s)
	}
}

// A rows page of a table with three relations (one to itself) and three
// rollups reads each linked table, its fields, the links and the linked
// cells once for the page, not once per relation or per linked table: at
// most ten round trips (about 22 before H7). A page of a table with no
// relations is logged beside it.
func TestRowsPageReadsLinksOncePerPage(t *testing.T) {
	f := newLinkedWorld(t, 20, 3)
	plain, pName := f.w.table("Plain")
	for i := range 20 {
		f.w.row(plain, map[string]interface{}{pName.Id.String(): strings.Repeat("p", i+1)})
	}
	calls := integration.CountStoreCalls(t, f.w.env, noGraph)
	read := func(tb *model.DataTable) *RowsWindow {
		calls.Reset()
		win, err := QueryRows(calls.Context(f.w.ctx), tb.Id, f.w.me, query.Spec{}, query.Page{Size: 100}, false)
		if err != nil {
			t.Fatalf("the %s page: %v", tb.Name, err)
		}
		settled(calls)
		return win
	}
	read(f.vendors) // warm: prepared statements and kept settings
	if win := read(f.vendors); len(win.Rows) != 20 {
		t.Fatalf("the vendors page holds %d rows, want 20", len(win.Rows))
	}
	said(t, "vendors page (3 relations, 3 rollups)", calls)
	trips, graph := calls.Trips(), calls.Graph()
	read(plain)
	said(t, "plain page (no relations)", calls)
	if graph != 0 {
		t.Errorf("the page called Dgraph %d times; tables keep nothing there", graph)
	}
	if trips > 10 {
		t.Errorf("the vendors page took %d round trips, want at most 10: one read of the linked tables, their fields, the links and the linked cells for the whole page", trips)
	}
}

// An empty page of a table with relations and rollups (its one row deleted,
// still linked from a project) holds no rows and reads no links and no
// linked cells: the batched reads are skipped, not sent with no ids.
func TestAnEmptyRowsPageReadsNoLinks(t *testing.T) {
	f := newLinkedWorld(t, 0, 0)
	calls := integration.CountStoreCalls(t, f.w.env, noGraph)
	win, err := QueryRows(calls.Context(f.w.ctx), f.vendors.Id, f.w.me, query.Spec{}, query.Page{Size: 100}, false)
	if err != nil {
		t.Fatalf("the empty vendors page: %v", err)
	}
	settled(calls)
	said(t, "empty vendors page (3 relations, 3 rollups)", calls)
	if len(win.Rows) != 0 {
		t.Fatalf("the vendors page holds %d rows, want none", len(win.Rows))
	}
	if n := calls.Sent("data_table_links"); n != 0 {
		t.Errorf("the empty page read links %d times, want none", n)
	}
	if trips := calls.Trips(); trips > 10 {
		t.Errorf("the empty page took %d round trips, want at most the full page's 10", trips)
	}
}

// A paste of 1,000 rows into a table with two relations links them a chunk
// at a time: at most ten statements per 100-row chunk in all (about 8,000
// before H7: two reads per row per relation to check the links, then an
// insert and a count per row per relation), and the links go in with one
// INSERT per relation.
func TestPasteLinksRowsAChunkAtATime(t *testing.T) {
	f := newLinkedWorld(t, 4, 1)
	const n, chunk = 1000, 100
	in := BatchInput{}
	for i := range n {
		in.Create = append(in.Create, RowInput{Values: map[string]interface{}{
			f.pName.Id.String():   "Pasted " + uuid.NewString()[:8],
			f.project.Id.String(): []interface{}{f.vendorRows[i%len(f.vendorRows)].String()},
			f.pSecret.Id.String(): []interface{}{f.hidden.String()},
		}})
	}
	calls := integration.CountStoreCalls(t, f.w.env, noGraph)
	out, err := Batch(calls.Context(f.w.ctx), f.projects.Id, in, f.w.me)
	if err != nil {
		t.Fatalf("the paste: %v", err)
	}
	settled(calls)
	said(t, "a 1,000-row paste over 2 relations", calls)
	if len(out.Created) != n {
		t.Fatalf("the paste made %d rows, want %d", len(out.Created), n)
	}
	if got := f.w.links(f.project.Id, out.Created[n-1].Id) + f.w.links(f.pSecret.Id, out.Created[n-1].Id); got != 2 {
		t.Fatalf("the last pasted row holds %d links, want 2", got)
	}
	if inserts := calls.Sent("INSERT INTO data_table_links"); inserts > 2 {
		t.Errorf("the paste inserted links in %d statements, want at most 2 (one a relation)", inserts)
	}
	if most := 10 * (n / chunk); calls.Statements() > most {
		t.Errorf("the paste sent %d statements, want at most %d (ten a %d-row chunk)", calls.Statements(), most, chunk)
	}
	// Every pasted row, on either side of each chunk's edge, links exactly
	// the vendor and the hidden row its cells named, each once.
	vendorsOf, hiddenOf := f.w.linksFrom(f.project.Id), f.w.linksFrom(f.pSecret.Id)
	for i, r := range out.Created {
		if got, want := vendorsOf[r.Id], f.vendorRows[i%len(f.vendorRows)]; len(got) != 1 || got[0] != want {
			t.Fatalf("pasted row %d links vendors %v, want [%s]", i, got, want)
		}
		if got := hiddenOf[r.Id]; len(got) != 1 || got[0] != f.hidden {
			t.Fatalf("pasted row %d links hidden rows %v, want [%s]", i, got, f.hidden)
		}
	}
}

// linksFrom is every link a field makes, by the row it is made from.
func (w *relationWorld) linksFrom(field uuid.UUID) map[uuid.UUID][]uuid.UUID {
	w.t.Helper()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(w.ctx, `SELECT from_row, to_row FROM data_table_links WHERE field_id = $1`, field)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	out := map[uuid.UUID][]uuid.UUID{}
	for rows.Next() {
		var from, to uuid.UUID
		if err := rows.Scan(&from, &to); err != nil {
			w.t.Fatal(err)
		}
		out[from] = append(out[from], to)
	}
	if err := rows.Err(); err != nil {
		w.t.Fatal(err)
	}
	return out
}

// A cell edit of a row that links to another table reads the table and its
// fields once and never reads its own row again after writing it: the
// write hands what it read and wrote down to the cell index, the links it
// tells and the autofill. What it reads of the tables it links to (their
// fields, links and cells, to work out the row's linked cells) is not its
// own and is bounded by the "table cell edit" store budget instead. A read
// is the edited table's or row's own when an argument of its own names it
// (StoreCalls.Named), whatever its text: the write's own UPDATE, naming the
// row in its list, shows the counter sees the row at all.
func TestCellEditRereadsNothingItHolds(t *testing.T) {
	f := newLinkedWorld(t, 2, 2)
	page, err := QueryRows(f.w.ctx, f.orders.Id, f.w.me, query.Spec{}, query.Page{Size: 1}, false)
	if err != nil || len(page.Rows) == 0 {
		t.Fatalf("an order to edit: %v", err)
	}
	order := page.Rows[0].Id
	edit := func(ctx context.Context, amount int) {
		if _, err := PatchRow(ctx, f.orders.Id, order, map[string]interface{}{f.amount.Id.String(): amount}, f.w.me); err != nil {
			t.Fatalf("the cell edit: %v", err)
		}
	}
	calls := integration.CountStoreCalls(t, f.w.env, noGraph)
	edit(f.w.ctx, 41) // warm
	settled(calls)
	edit(calls.Context(f.w.ctx), 42)
	settled(calls)
	said(t, "a cell edit of a linked row", calls)
	table, row := f.orders.Id.String(), order.String()
	for _, s := range calls.Naming(table) {
		t.Logf("  names the table: %s", s)
	}
	for _, s := range calls.Naming(row) {
		t.Logf("  names the row: %s", s)
	}
	if n := calls.Named(table, "FROM data_table_fields"); n != 1 {
		t.Errorf("the edit read the table's own fields %d times, want once", n)
	}
	if n := calls.Named(table, "FROM data_tables"); n != 1 {
		t.Errorf("the edit read the table itself %d times, want once", n)
	}
	if n := calls.NamedIn(row, "UPDATE data_table_rows"); n != 1 {
		t.Fatalf("the edit's write named the row %d times, want once: the counter can't see the row", n)
	}
	written := calls.NamedIn(row, "SELECT", "FROM data_table_rows", "UPDATE data_table_rows")
	if n := calls.NamedIn(row, "SELECT", "FROM data_table_rows") - written; n != 0 {
		t.Errorf("the edit read its own row %d times besides writing it, want none", n)
	}
}
