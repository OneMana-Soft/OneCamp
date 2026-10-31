//go:build integration

package integration_test

import (
	"context"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
)

// computedStoreMigration is the Computed Store's migration number, and the
// one before it, read from the files: the integrator may renumber it at
// landing.
func computedStoreMigration(t *testing.T) (dir string, mine, before uint) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	dir = filepath.Join(filepath.Dir(here), "..", "..", "migrations")
	ups, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var all []uint
	for _, f := range ups {
		name := filepath.Base(f)
		n, err := strconv.ParseUint(strings.SplitN(name, "_", 2)[0], 10, 32)
		if err != nil {
			continue
		}
		all = append(all, uint(n))
		if strings.HasSuffix(name, "_data_table_computed.up.sql") {
			mine = uint(n)
		}
	}
	if mine == 0 {
		t.Fatal("no *_data_table_computed.up.sql among the migrations")
	}
	for _, n := range all {
		if n < mine && n > before {
			before = n
		}
	}
	return dir, mine, before
}

// The Computed Store's migration (airtable-03) adds the codebase's first
// trigger, and the store writes formula and rollup values into
// data_table_cell_index. Its down takes all of it out again: no trigger stays
// on data_table_fields, and no kept value stays in the index, since code from
// before it never wrote one there. Up again, every formula and rollup is held
// once more, unfilled, for the sweep's first fill.
func TestComputedStoreMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.SetupDgraph(t)
	me := uuid.New()
	if _, err := env.PG.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'computed-migration@example.test', 'computed-migration', 'computed-migration', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Budgets", Visibility: "workspace"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Budget", Type: model.FieldNumber}, owner)
	if err != nil {
		t.Fatal(err)
	}
	double, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Double", Type: model.FieldFormula, Config: map[string]interface{}{"formula": "{Budget} * 2"}}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataTable.CreateRow(ctx, tb.Id, dataTable.RowInput{Values: map[string]interface{}{budget.Id.String(): 21}}, owner); err != nil {
		t.Fatal(err)
	}
	if err := rowQuery.RecomputeKept(ctx); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) (n int) {
		t.Helper()
		if err := env.PG.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const kept = `SELECT count(*) FROM data_table_cell_index WHERE field_id = $1`
	const triggers = `SELECT count(*) FROM pg_trigger WHERE tgrelid = 'data_table_fields'::regclass AND tgname LIKE 'data_table_computed%'`
	if count(kept, double.Id) == 0 {
		t.Fatal("the sweep kept no value of Double, so the down would have nothing to take out")
	}

	dir, mine, before := computedStoreMigration(t)
	m, err := migrate.New("file://"+dir, env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if v, _, _ := m.Version(); v < mine {
		t.Fatalf("migrations stop at %d, before the Computed Store's (%d)", v, mine)
	}
	if err := m.Migrate(before); err != nil {
		t.Fatalf("down past %d: %v", mine, err)
	}
	if n := count(kept, double.Id); n != 0 {
		t.Errorf("down, %d of Double's kept values stay in the cell index", n)
	}
	if n := count(triggers); n != 0 {
		t.Errorf("down, %d of the store's triggers stay on data_table_fields", n)
	}
	if n := count(`SELECT count(*) FROM pg_proc WHERE proname = 'data_table_computed_field_written'`); n != 0 {
		t.Errorf("down, the trigger's function is still there")
	}
	if n := count(`SELECT (to_regclass('data_table_computed') IS NOT NULL)::int + (to_regclass('data_table_computed_dirty') IS NOT NULL)::int`); n != 0 {
		t.Errorf("down, %d of the store's tables are still there", n)
	}

	if err := m.Migrate(mine); err != nil {
		t.Fatalf("up to %d: %v", mine, err)
	}
	var gen, filled int64
	if err := env.PG.QueryRowContext(ctx, `SELECT gen, filled_gen FROM data_table_computed WHERE field_id = $1`, double.Id).Scan(&gen, &filled); err != nil {
		t.Fatalf("up again, Double isn't held: %v", err)
	}
	if filled == gen {
		t.Errorf("up again, Double counts as filled before any fill")
	}
	if n := count(triggers); n != 2 {
		t.Errorf("up again, %d of the store's 2 triggers are on data_table_fields", n)
	}
}
