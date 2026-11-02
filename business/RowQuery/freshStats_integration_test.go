//go:build integration

package business_test

// V-107 (main, 30 Oct): a fresh 100,000-row table sorted by a formula made
// after its import wouldn't load live. The formula's first fill writes an
// entry a row to the cell index, which the planner hadn't counted: Postgres
// 12 counts an insert-only table again only at the autovacuum's next look,
// so until then it took the field for a handful of entries, and the first
// window (joined to them, in their order), its count and its sums planned
// badly and ran past the statement timeout. The server counts them itself
// once the fill is done, before the field answers from the store: the
// planner sees the field's entries, and the first window with its count and
// sums answers within production's statement timeout. A stored field made
// over the rows already there is counted the same way, beside its request.
//
// Run: integ23.sh <worktree> <log> '^TestAFreshFormulaSortedViewAnswersAtOnce$' with PKGS='./business/RowQuery/'

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// plannedEntries is how many of a field's cell index entries the planner
// expects: what it has counted, not what is there.
func plannedEntries(ctx context.Context, t *testing.T, field uuid.UUID) float64 {
	t.Helper()
	var raw string
	// A literal, so the plan is for this field's own value.
	q := fmt.Sprintf(`EXPLAIN (FORMAT JSON) SELECT 1 FROM data_table_cell_index WHERE field_id = '%s'`, field)
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, q).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan []struct {
		Plan struct {
			Rows float64 `json:"Plan Rows"`
		} `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &plan); err != nil || len(plan) == 0 {
		t.Fatalf("plan %q: %v", raw, err)
	}
	return plan[0].Plan.Rows
}

// noAutovacuumOn keeps the autovacuum off tables until the test ends, so the
// planner knows of them only what the server, or the test, counted.
func noAutovacuumOn(ctx context.Context, t *testing.T, tables ...string) {
	t.Helper()
	for _, table := range tables {
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `ALTER TABLE `+table+` SET (autovacuum_enabled = false)`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = postgresInit.DBConn.SqlDB.ExecContext(context.Background(), `ALTER TABLE `+table+` RESET (autovacuum_enabled)`)
		})
	}
}

// countForThePlanner has the planner count the tables a test fills at once
// (the rows, their links and the cell index) as they stand, before the test
// measures what its reads cost: the rows and the cell index as the server's
// count after a fill does (model.VacuumRows), the links as they stand until
// their upkeep's next vacuum (an ANALYZE). What it measures then never hangs
// on the server's count after a fill (CountAfterFill), which only logs a
// failure, or on when the autovacuum looks.
func countForThePlanner(ctx context.Context, t *testing.T) {
	t.Helper()
	start := time.Now()
	if err := model.VacuumRows(ctx); err != nil {
		t.Fatalf("the planner's count of the rows and the cell index: %v", err)
	}
	t.Logf("the planner's count (model.VacuumRows) took %v", time.Since(start).Round(time.Millisecond))
	for _, table := range []string{"data_table_rows", "data_table_cell_index"} {
		whole, pages := pagesWhole(ctx, t, table)
		t.Logf("the planner's count: %d of %s's %d pages whole", whole, table, pages)
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `ANALYZE data_table_links`); err != nil {
		t.Fatalf("the planner's count of the links: %v", err)
	}
}

// pagesWhole is how many of table's pages the planner takes for whole
// (pg_class.relallvisible), of how many it counts (relpages).
func pagesWhole(ctx context.Context, t *testing.T, table string) (whole, pages int64) {
	t.Helper()
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT relallvisible, relpages FROM pg_class WHERE oid = to_regclass($1)`, table).Scan(&whole, &pages); err != nil {
		t.Fatalf("%s's counts: %v", table, err)
	}
	return whole, pages
}

// sweepUntilFilled runs the sweep until field's kept entries are filled at
// its generation, as a kind of value; what names the field in a failure.
func sweepUntilFilled(ctx context.Context, t *testing.T, field uuid.UUID, what string) {
	t.Helper()
	filled := func() bool {
		var gen, done int64
		var kind string
		err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT gen, filled_gen, kind FROM data_table_computed WHERE field_id = $1`, field).Scan(&gen, &done, &kind)
		return err == nil && gen == done && kind != ""
	}
	for pass := 0; !filled(); pass++ {
		if pass == 20 {
			t.Fatalf("%s was never filled", what)
		}
		if err := rowQuery.RecomputeKept(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAFreshFormulaSortedViewAnswersAtOnce(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	statementsGetAMinute(t)
	integration.SetupDgraph(t)
	db := postgresInit.DBConn.SqlDB
	// The autovacuum's look is what this server mustn't wait for.
	noAutovacuumOn(ctx, t, "data_table_cell_index")
	me := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'fresh@example.test', 'fresh', 'Fresh', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Launch budget", Visibility: "workspace"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	name, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Name", Type: model.FieldText}, owner)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Budget", Type: model.FieldNumber}, owner)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, computedProjects)
	for i := range ids {
		ids[i] = uuid.New()
	}
	// Imported, and made whole as an import's table is (counted then).
	fill(ctx, t, tb.Id, me, ids, func(i int) map[string]interface{} {
		return map[string]interface{}{name.Id.String(): fmt.Sprintf("Line %06d", i), budget.Id.String(): float64((i * 104729) % 100000)}
	})
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatal(err)
	}

	// Then a formula, made after the import, and its first fill.
	double, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Double", Type: model.FieldFormula, Config: map[string]interface{}{"formula": "{Budget} * 2"}}, owner)
	if err != nil {
		t.Fatal(err)
	}
	sweepUntilFilled(ctx, t, double.Id, "Double")

	// The planner counted the fill: it expects the field's entries, a row each.
	if got := plannedEntries(ctx, t, double.Id); got < computedProjects/2 {
		t.Errorf("after Double's first fill the planner expects %.0f of its %d cell index entries: the table wasn't counted again, so a window sorted by it plans for a handful", got, computedProjects)
	}

	// A stored field made over the rows already there (CreateField ->
	// IndexField) writes an entry a row too, empty ones included; it's
	// counted beside the request that made it, so it's waited for here.
	spend, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Spend", Type: model.FieldNumber}, owner)
	if err != nil {
		t.Fatal(err)
	}
	var planned float64
	for until := time.Now().Add(30 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		if planned = plannedEntries(ctx, t, spend.Id); planned >= computedProjects/2 || time.Now().After(until) {
			break
		}
	}
	if planned < computedProjects/2 {
		t.Errorf("30 s after Spend was made over %d rows the planner expects %.0f of its entries: they weren't counted", computedProjects, planned)
	}
	// A field made raises every kept field of its table (migration 535), so
	// Double is filled again before the store answers by it.
	sweepUntilFilled(ctx, t, double.Id, "after Spend was made, Double")

	// The page's first window, sorted by Double, with its count and its sum,
	// at production's statement timeout.
	postgresInit.DBConn.DBTimeout = 3 * time.Second
	f := "field:" + double.Id.String()
	spec := query.Spec{
		Scope:     query.Scope{Kind: query.ScopeTable, ID: tb.Id.String()},
		Sorts:     []query.Sort{{Field: f, Desc: true}},
		Summaries: []query.SummaryRequest{{Field: f, Fns: []string{"sum"}}},
	}
	start := time.Now()
	res, err := query.Queries.Run(rowQuery.WithCount(ctx), accessBusiness.Subject{UserUUID: me.String()}, spec, query.Page{Size: windowSize})
	took := time.Since(start)
	if err != nil {
		t.Fatalf("the first window sorted by Double: %v (after %v)", err, took)
	}
	if len(res.IDs) != windowSize || res.Total != computedProjects {
		t.Errorf("the first window sorted by Double: %d rows of %d, where %d of %d are", len(res.IDs), res.Total, windowSize, computedProjects)
	}
	if res.Summaries == nil {
		t.Errorf("the first window sorted by Double came without its sum (left out past its budget, after %v)", took)
	}
	t.Logf("first window, count and sum in %v", took)
}

// V-150 (main, 31 Oct): Postgres 12 vacuums a table for the rows its writes
// left dead, never for rows added, so after an import the visibility map
// holds none of the pages its rows and their cell index entries went to. An
// ANALYZE counts them, but takes the map's count of whole pages as it finds
// it (pg_class.relallvisible): none. So the planner costs each read of an
// index there as a read of the table's page too, and a window filtered and
// sorted by stored numbers over 100,000 rows was planned at times as a read
// of every row (TestComputedFieldsSortAndFilterAt100k's CONTROL). The
// server's count after a fill (CountAfterFill) vacuums the two tables as it
// counts them: the pages the fill wrote are whole for the planner.
//
// The harness's server doesn't wait for the disk at commit (synchronous_commit
// off), and a VACUUM marks a page whole only once the commits that wrote it
// are on disk. So this also proves the count flushes the fill's commits first
// (model.VacuumRows): without the flush, none of the cell index's 205 pages
// were marked when it ran alone, and 122 of 205 after the fresh-formula test,
// split where the fill's transactions crossed a 32-transaction group of the
// commit log (31 Oct, TABLES-LEAD-108).
//
// Run: integ_rel.sh <worktree> <log> '^TestAFilledTableIsWholeForThePlanner$' with PKGS='./business/RowQuery/'
func TestAFilledTableIsWholeForThePlanner(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	statementsGetAMinute(t)
	integration.SetupDgraph(t)
	db := postgresInit.DBConn.SqlDB
	// Only the server's count may mark the pages whole here.
	noAutovacuumOn(ctx, t, "data_table_rows", "data_table_cell_index")
	me := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'whole@example.test', 'whole', 'Whole', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Imported vendors", Visibility: "workspace"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	name, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Name", Type: model.FieldText}, owner)
	if err != nil {
		t.Fatal(err)
	}
	spend, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Spend", Type: model.FieldNumber}, owner)
	if err != nil {
		t.Fatal(err)
	}
	const rows = 5000
	ids := make([]uuid.UUID, rows)
	for i := range ids {
		ids[i] = uuid.New()
	}
	fill(ctx, t, tb.Id, me, ids, func(i int) map[string]interface{} {
		return map[string]interface{}{name.Id.String(): fmt.Sprintf("Vendor %05d", i), spend.Id.String(): float64((i * 7919) % 50000)}
	})
	// Made whole as an import's table is, and counted then.
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatal(err)
	}

	// The database is the test's own: the fill's pages are nearly all of them.
	for _, table := range []string{"data_table_rows", "data_table_cell_index"} {
		whole, pages := pagesWhole(ctx, t, table)
		if pages == 0 {
			t.Errorf("after a fill of %d rows the planner counts no pages of %s: it wasn't counted", rows, table)
			continue
		}
		if whole < pages*9/10 {
			t.Errorf("after a fill of %d rows the planner takes %d of %s's %d pages for whole: it costs each read of an index there as a read of the table too", rows, whole, table, pages)
		}
		t.Logf("%s: %d of %d pages whole", table, whole, pages)
	}
}
