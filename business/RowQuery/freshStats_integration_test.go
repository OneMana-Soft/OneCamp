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

// noAutovacuumOnTheCellIndex keeps the autovacuum off the cell index until
// the test ends, so the planner knows of it only what the server counted.
func noAutovacuumOnTheCellIndex(ctx context.Context, t *testing.T) {
	t.Helper()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `ALTER TABLE data_table_cell_index SET (autovacuum_enabled = false)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = postgresInit.DBConn.SqlDB.ExecContext(context.Background(), `ALTER TABLE data_table_cell_index RESET (autovacuum_enabled)`)
	})
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
	noAutovacuumOnTheCellIndex(ctx, t)
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
