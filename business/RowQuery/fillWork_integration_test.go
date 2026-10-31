//go:build integration

package business_test

// V-114 (main, 30 Oct): a formula's first fill over a live 100,000-row table
// took 74 s (1,351 rows/s), slower with every batch, and the table's views
// timed out while it ran. Each batch of 500 rows deleted its rows' entries of
// the field before writing them, and the planner, which hadn't counted a new
// field's entries, read them along the field's own index and kept the batch's
// rows: every entry written so far, on each batch, to delete nothing. So a
// fill read more for each row the bigger its table, as did a plain field made
// over a table's rows (IndexField), written the same way. Each entry is now
// written over its cell's own, and only a cell left with none loses its old
// one: what a fill reads for each row is the same at 10,000 rows as at 40,000,
// measured as work, which a loaded machine doesn't change. Both fields are
// made as the web makes them (POST /tables/{id}/fields), and after the fill
// the table's sum by the formula is answered over HTTP (POST
// /tables/{id}/query). A cell a field's new type leaves with no entry still
// loses its old one.
//
// Run: integ23.sh <worktree> <log> '^TestAFillReadsAsMuchForEachRowAtAnySize$' with PKGS='./business/RowQuery/'

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// smallFill and bigFill are the two tables' rows, four times apart.
const smallFill, bigFill = 10000, 40000

// readsReported is readsNow with every session's counts in it. A Postgres 12
// session reports its counts at a transaction's end, at most twice a second,
// and keeps the rest until a later one or its own end, so an idle session of
// the server's pool can hold the last of a fill's reads. A session reports
// all it has as it ends: the pool (connected by dsn, which names its
// sessions, so they're told from the harness's) is closed, opened again as
// it was, and each of its old sessions waited for until it's gone.
func readsReported(ctx context.Context, t *testing.T, dsn string) reads {
	t.Helper()
	old, timeout := postgresInit.DBConn.SqlDB, postgresInit.DBConn.DBTimeout
	var closing time.Time
	var name string
	if err := old.QueryRowContext(ctx, `SELECT clock_timestamp(), current_setting('application_name')`).Scan(&closing, &name); err != nil {
		t.Fatal(err)
	}
	if name == "" {
		t.Fatal("the server's pool has no application_name of its own to tell its sessions by")
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := postgresInit.ConnectPostgres(ctx, dsn); err != nil {
		t.Fatalf("connect: %v", err)
	}
	postgresInit.DBConn.DBTimeout = timeout
	for start := time.Now(); ; time.Sleep(50 * time.Millisecond) {
		var open int
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND application_name = $2 AND backend_start <= $1`, closing, name).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open == 0 {
			break
		}
		if time.Since(start) > 30*time.Second {
			t.Fatalf("%d sessions of the closed pool were still open after 30 s", open)
		}
	}
	return readsNow(ctx, t)
}

func TestAFillReadsAsMuchForEachRowAtAnySize(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	dsn := env.DSN + "&application_name=fill_work"
	if err := postgresInit.ConnectPostgres(ctx, dsn); err != nil {
		t.Fatalf("connect: %v", err)
	}
	statementsGetAMinute(t)
	integration.SetupDgraph(t)
	// The planner knows of the cell index what the server counted after each
	// fill, whenever the autovacuum looks.
	noAutovacuumOnTheCellIndex(ctx, t)
	me := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'fills@example.test', 'fills', 'Fills', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}

	// sized is a table imported with an Amount and a Tag a row (every other
	// row's empty), indexed whole, and what its fills read for each row.
	type sized struct {
		rows                    int
		tb                      *model.DataTable
		tag                     *model.Field
		double, spend           uuid.UUID
		doubleReads, spendReads float64
	}
	small, big := &sized{rows: smallFill}, &sized{rows: bigFill}
	for _, s := range []*sized{small, big} {
		var err error
		if s.tb, err = dataTable.CreateTable(ctx, dataTable.TableInput{Name: fmt.Sprintf("Ledger of %d", s.rows), Visibility: "workspace"}, owner); err != nil {
			t.Fatal(err)
		}
		amount, err := dataTable.CreateField(ctx, s.tb.Id, dataTable.FieldInput{Name: "Amount", Type: model.FieldNumber}, owner)
		if err != nil {
			t.Fatal(err)
		}
		if s.tag, err = dataTable.CreateField(ctx, s.tb.Id, dataTable.FieldInput{Name: "Tag", Type: model.FieldText}, owner); err != nil {
			t.Fatal(err)
		}
		ids := make([]uuid.UUID, s.rows)
		for i := range ids {
			ids[i] = uuid.New()
		}
		fill(ctx, t, s.tb.Id, me, ids, func(i int) map[string]interface{} {
			cells := map[string]interface{}{amount.Id.String(): float64(i % 1000)}
			if i%2 == 0 {
				cells[s.tag.Id.String()] = "urgent"
			}
			return cells
		})
		if err := rowQuery.IndexTable(ctx, s.tb.Id); err != nil {
			t.Fatal(err)
		}
	}

	// readFor is what Postgres read of the cell index and the rows for each
	// of s's rows while do ran: entries along an index, and rows through.
	readFor := func(s *sized, do func()) float64 {
		t.Helper()
		before := readsReported(ctx, t, dsn)
		do()
		after := readsReported(ctx, t, dsn)
		return float64(after.indexTuples+after.seqTuples-before.indexTuples-before.seqTuples) / float64(s.rows)
	}
	// madeOverHTTP makes a field on s's table as the web does (POST
	// /tables/{id}/fields), and answers its id.
	madeOverHTTP := func(s *sized, body map[string]any) uuid.UUID {
		t.Helper()
		rec := overHTTP(t, dataTableController.CreateField, "/tables/"+s.tb.Id.String()+"/fields", s.tb.Id, me, body)
		var out struct {
			Data struct {
				ID uuid.UUID `json:"id"`
			} `json:"data"`
		}
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Data.ID == uuid.Nil {
			t.Fatalf("POST /tables/{id}/fields %v: %d %s", body["name"], rec.Code, rec.Body.String())
		}
		return out.Data.ID
	}
	// entries is how many entries field has, and what their numbers add up to.
	entries := func(field uuid.UUID) (n int, sum float64) {
		t.Helper()
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(n), 0) FROM data_table_cell_index WHERE field_id = $1`, field).Scan(&n, &sum); err != nil {
			t.Fatal(err)
		}
		return n, sum
	}

	// A formula made after the import, and its first fill.
	for _, s := range []*sized{small, big} {
		s.double = madeOverHTTP(s, map[string]any{"name": "Double", "type": model.FieldFormula, "config": map[string]any{"formula": "{Amount} * 2"}})
		s.doubleReads = readFor(s, func() { sweepUntilFilled(ctx, t, s.double, fmt.Sprintf("Double over %d rows", s.rows)) })
	}
	// A plain field made over the rows (CreateField -> IndexField): an empty
	// entry a row, written as a fill's are.
	for _, s := range []*sized{small, big} {
		s.spendReads = readFor(s, func() { s.spend = madeOverHTTP(s, map[string]any{"name": "Spend", "type": model.FieldNumber}) })
	}
	t.Logf("read for each row: Double's first fill %.2f at %d rows, %.2f at %d; Spend made %.2f, %.2f", small.doubleReads, small.rows, big.doubleReads, big.rows, small.spendReads, big.spendReads)

	for _, s := range []*sized{small, big} {
		// Each row's Double, twice its Amount (0 to 999, a thousand rows
		// at a time), in the index and in the table's sum over HTTP, and
		// Spend's empty entry.
		want := 2 * float64(s.rows/1000) * 499500
		if n, sum := entries(s.double); n != s.rows || sum != want {
			t.Errorf("Double's first fill over %d rows wrote %d entries adding up to %.0f, where each row's is twice its Amount (%.0f)", s.rows, n, sum, want)
		}
		f := "field:" + s.double.String()
		code, out := queryOverHTTP(t, s.tb.Id, me, map[string]any{"spec": query.Spec{Summaries: []query.SummaryRequest{{Field: f, Fns: []string{"sum"}}}}, "n": 1, "count": true})
		if code != http.StatusOK || out.Data.Total != s.rows || out.Data.Summaries[f]["sum"] != want {
			t.Errorf("summed over HTTP after the fill: %d %q, %d rows, Double's sum %v, where its %d rows add up to %.0f", code, out.Msg, out.Data.Total, out.Data.Summaries[f]["sum"], s.rows, want)
		}
		if n, _ := entries(s.spend); n != s.rows {
			t.Errorf("Spend, made over %d rows, has %d entries, where each row has an empty one", s.rows, n)
		}
	}
	// The same for each row at any size: a quarter more at four times the
	// rows (and one for rounding) passes, where a fill that read everything
	// it had written so far reads four times as much.
	if big.doubleReads > small.doubleReads*1.25+1 {
		t.Errorf("Double's first fill read %.2f entries and rows for each of %d rows, and %.2f for each of %d: what a fill reads for a row grows with its table", small.doubleReads, small.rows, big.doubleReads, big.rows)
	}
	if big.spendReads > small.spendReads*1.25+1 {
		t.Errorf("Spend, made over %d rows, read %.2f entries and rows for each, and %.2f for each of %d: what a field made reads for a row grows with its table", small.rows, small.spendReads, big.spendReads, big.rows)
	}

	// Tag made a multi-select: a tagged row's cell has its label, and an
	// untagged row's, empty, has no entry now (a multi-select's empty cell
	// has none), so the empty one it had as a text is gone.
	multi := model.FieldMultiSelect
	if _, err := dataTable.UpdateField(ctx, small.tb.Id, small.tag.Id, dataTable.FieldPatch{Type: &multi}, owner); err != nil {
		t.Fatal(err)
	}
	var kept, labelled int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE a = '{urgent}')
		FROM data_table_cell_index WHERE field_id = $1`, small.tag.Id).Scan(&kept, &labelled); err != nil {
		t.Fatal(err)
	}
	if kept != small.rows/2 || labelled != kept {
		t.Errorf("Tag, made a multi-select over %d rows (every other one tagged), has %d entries, %d of them its label, where the %d tagged rows have one each and the rest none", small.rows, kept, labelled, small.rows/2)
	}
}
