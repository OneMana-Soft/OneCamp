//go:build integration
// +build integration

package integration_test

// Migration 301 (tables A2, the row ledger) against Postgres 12 with 100,000
// rows (G-MIGRATE): down, then up again. The up gives every row, deleted ones
// too, a key in the order rows were read in before it (position, then
// created_at, then id), so no table's rows move; the down takes the ledger
// away and leaves every row's cells, place and delete as they were.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableMigrationRowLedger -v

import (
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	position "github.com/akashc777/OneCamp/business/Tree/position"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func TestTableMigrationRowLedger(t *testing.T) {
	w := newTableWorld(t, "amara")
	big := w.table("amara", "Big")
	small := w.table("amara", "Small")
	notes := w.field("amara", big, "Notes", dataTableModel.FieldText, nil)
	const rows = 100000
	start := time.Now()
	// Places that tie (position % 7, created_at in threes), deleted rows among
	// them: the order is position, then created_at, then id.
	if _, err := w.env.PG.Exec(`INSERT INTO data_table_rows (id, table_id, values, position, created_at, deleted_at)
		SELECT md5(g::text || $1::text)::uuid, $1::text::uuid, jsonb_build_object($2::text, 'row ' || g), (g % 7)::float,
			TIMESTAMPTZ '2026-01-01' + (g / 3) * INTERVAL '1 second',
			CASE WHEN g % 50 = 0 THEN NOW() END
		FROM generate_series(1, $3) g`, big.Id.String(), notes, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := w.env.PG.Exec(`INSERT INTO data_table_rows (id, table_id, values, position)
		SELECT md5('small' || g::text)::uuid, $1::text::uuid, '{}'::jsonb, 10 - g FROM generate_series(1, 5) g`, small.Id.String()); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded %d rows in %v", rows+5, time.Since(start))

	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "migrations")
	matches, err := filepath.Glob(filepath.Join(dir, "*_table_row_ledger.up.sql"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("the migration: %v %v", matches, err)
	}
	n, err := strconv.Atoi(strings.SplitN(filepath.Base(matches[0]), "_", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.New("file://"+dir, w.env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	start = time.Now()
	for v, _, _ := m.Version(); v >= uint(n); v, _, _ = m.Version() {
		if err := m.Steps(-1); err != nil {
			t.Fatalf("down from %d: %v", v, err)
		}
	}
	t.Logf("down in %v", time.Since(start))
	var cols, tables int
	if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM information_schema.columns
		WHERE (table_name = 'data_table_rows' AND column_name IN ('version', 'updated_by', 'deleted_by', 'position_key'))
		   OR (table_name IN ('data_tables', 'data_table_fields') AND column_name = 'deleted_by')`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("after the down %d of the columns are left (%v)", cols, err)
	}
	if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM information_schema.tables
		WHERE table_name IN ('data_table_change_sets', 'data_table_changes')`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("after the down %d of the ledger's tables are left (%v)", tables, err)
	}
	var left, deleted int
	if err := w.env.PG.QueryRow(`SELECT COUNT(*), COUNT(deleted_at) FROM data_table_rows WHERE table_id = $1`, big.Id).Scan(&left, &deleted); err != nil || left != rows || deleted != rows/50 {
		t.Fatalf("the down left %d rows, %d deleted (%v)", left, deleted, err)
	}

	start = time.Now()
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	t.Logf("up again in %v", time.Since(start))

	// Every row has a key the scheme makes, one to a row of each table, and
	// its rows, deleted ones too, come in the order they came in before.
	var unkeyed, invalid, moved int
	keys, err := w.env.PG.Query(`SELECT position_key FROM data_table_rows WHERE table_id = ANY($1::uuid[])`, "{"+big.Id.String()+","+small.Id.String()+"}")
	if err != nil {
		t.Fatal(err)
	}
	for keys.Next() {
		var k *string
		if err := keys.Scan(&k); err != nil {
			t.Fatal(err)
		}
		switch {
		case k == nil:
			unkeyed++
		case !position.ValidPosition(*k):
			invalid++
		}
	}
	keys.Close()
	if unkeyed != 0 || invalid != 0 {
		t.Errorf("after the up %d rows have no place and %d one the scheme can't make", unkeyed, invalid)
	}
	if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM (
			SELECT id, row_number() OVER (PARTITION BY table_id ORDER BY position, created_at, id) AS before,
				row_number() OVER (PARTITION BY table_id ORDER BY position_key, id) AS after
			FROM data_table_rows WHERE table_id = ANY($1::uuid[])
		) o WHERE before <> after`, "{"+big.Id.String()+","+small.Id.String()+"}").Scan(&moved); err != nil || moved != 0 {
		t.Errorf("%d rows moved (%v)", moved, err)
	}
	var distinct int
	if err := w.env.PG.QueryRow(`SELECT COUNT(DISTINCT position_key) FROM data_table_rows WHERE table_id = $1`, big.Id).Scan(&distinct); err != nil || distinct != rows {
		t.Errorf("%d rows have %d places (%v)", rows, distinct, err)
	}
	var notesKept int
	if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM data_table_rows WHERE table_id = $1 AND values ->> $2 LIKE 'row %'`, big.Id, notes).Scan(&notesKept); err != nil || notesKept != rows {
		t.Errorf("%d rows kept their cells (%v)", notesKept, err)
	}

	// The rows read in that order, page after page, by the order index; and a
	// new row goes after every row, deleted ones too.
	first, err := dataTableModel.ListRows(w.ctx, small.Id, 10, 0)
	if err != nil || len(first) != 5 || first[0].Position != 5 || first[4].Position != 9 {
		t.Fatalf("the small table reads %d rows (%v)", len(first), err)
	}
	r, err := dataTableBusiness.CreateRow(w.ctx, small.Id, dataTableBusiness.RowInput{}, w.people["amara"])
	if err != nil {
		t.Fatal(err)
	}
	if r.PositionKey <= first[4].PositionKey {
		t.Errorf("a new row's place %q is before the last row's %q", r.PositionKey, first[4].PositionKey)
	}
}
