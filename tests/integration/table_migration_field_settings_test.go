//go:build integration
// +build integration

package integration_test

// Migration 300 (tables A1, G-MIGRATE): up, down and up again on Postgres 12
// over a table of 100,000 rows, deleted ones among them. Each pass is timed
// (logged, never asserted: the machine's load isn't the migration's). After
// it, every row is numbered in the order rows were made, every table has the
// primary field it had, and nothing else changed.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableMigrationFieldSettings -v

import (
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func TestTableMigrationFieldSettings(t *testing.T) {
	w := newTableWorld(t, "amara")
	tb := w.table("amara", "Big")
	notes := w.field("amara", tb, "Notes", dataTableModel.FieldText, nil)
	const rows = 100000
	start := time.Now()
	if _, err := w.env.PG.Exec(`INSERT INTO data_table_rows (id, table_id, values, position, created_at, deleted_at)
		SELECT md5(g::text || $1::text)::uuid, $1::text::uuid, jsonb_build_object($2::text, 'row ' || g), (g % 7)::float,
			TIMESTAMPTZ '2026-01-01' + (g / 3) * INTERVAL '1 second',
			CASE WHEN g % 50 = 0 THEN NOW() END
		FROM generate_series(1, $3) g`, tb.Id.String(), notes, rows); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded %d rows in %v", rows, time.Since(start))

	// How the up names every row: in the order they were made.
	mismatched := func() int {
		var n int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM data_table_rows r JOIN (
				SELECT id, row_number() OVER (PARTITION BY table_id ORDER BY created_at, position, id) AS no FROM data_table_rows
			) o ON o.id = r.id
			WHERE r.table_id = $1 AND r.row_no IS DISTINCT FROM o.no`, tb.Id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var primary string
	if err := w.env.PG.QueryRow(`SELECT id FROM data_table_fields WHERE table_id = $1 AND is_primary`, tb.Id).Scan(&primary); err != nil {
		t.Fatalf("the table's primary field: %v", err)
	}

	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "migrations")
	matches, err := filepath.Glob(filepath.Join(dir, "*_table_field_settings.up.sql"))
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
	var cols int
	if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM information_schema.columns
		WHERE (table_name = 'data_table_rows' AND column_name = 'row_no')
		   OR (table_name = 'data_table_fields' AND column_name IN ('description', 'required', 'default_value', 'is_primary'))
		   OR (table_name = 'data_tables' AND column_name = 'next_row_no')`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("after the down %d of the columns are left (%v)", cols, err)
	}
	var left int
	if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM data_table_rows WHERE table_id = $1`, tb.Id).Scan(&left); err != nil || left != rows {
		t.Fatalf("the down left %d rows (%v)", left, err)
	}

	start = time.Now()
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	t.Logf("up again in %v", time.Since(start))
	if bad := mismatched(); bad != 0 {
		t.Errorf("%d rows aren't numbered in the order they were made", bad)
	}
	var again string
	if err := w.env.PG.QueryRow(`SELECT id FROM data_table_fields WHERE table_id = $1 AND is_primary`, tb.Id).Scan(&again); err != nil || again != primary {
		t.Errorf("the primary field is %s (%v), was %s", again, err, primary)
	}
	var next int64
	if err := w.env.PG.QueryRow(`SELECT next_row_no FROM data_tables WHERE id = $1`, tb.Id).Scan(&next); err != nil || next != rows+1 {
		t.Errorf("the table counts on from %d (%v), want %d", next, err, rows+1)
	}
	var notesKept int
	if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM data_table_rows WHERE table_id = $1 AND values ->> $2 LIKE 'row %'`, tb.Id, notes).Scan(&notesKept); err != nil || notesKept != rows {
		t.Errorf("%d rows kept their cells (%v)", notesKept, err)
	}
	// The kinds the programme builds are in the database's list; the
	// registry still refuses each until it's registered.
	if _, err := w.env.PG.Exec(`INSERT INTO data_table_fields (table_id, name, type) VALUES ($1, 'Rating', 'rating')`, tb.Id); err != nil {
		t.Errorf("a field of a kind to come doesn't fit the widened list: %v", err)
	}
}
