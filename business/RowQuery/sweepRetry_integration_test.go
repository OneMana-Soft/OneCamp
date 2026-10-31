//go:build integration

package business_test

// The repair sweep (tables.cellindex.repair) goes past a table it can't
// index. A pass took the tables without a whole index oldest first and
// stopped at the first that failed, so one such table (a long text's did,
// before T5c) held up every table made after it, and its error was logged
// at every pass. Now the pass records the failure on the table, leaves the
// table until its wait ends, indexes the tables after it, and says so once.
//
// Run: integ_rel.sh <worktree> <log> '^(TestTheRepairSweepGoesPastATableItCantIndex)$' with PKGS='./business/RowQuery/'

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// heard is a log that keeps the messages written to it.
type heard struct {
	mu   sync.Mutex
	said []string
}

func (h *heard) Enabled(context.Context, slog.Level) bool { return true }

func (h *heard) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.said = append(h.said, r.Message)
	return nil
}

func (h *heard) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *heard) WithGroup(string) slog.Handler      { return h }

// naming is how many messages named s.
func (h *heard) naming(s string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.said {
		if strings.Contains(m, s) {
			n++
		}
	}
	return n
}

func TestTheRepairSweepGoesPastATableItCantIndex(t *testing.T) {
	ctx, owner := tableOwner(t)
	db := postgresInit.DBConn.SqlDB
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// A table of a text field and one row, made age ago, its index not whole.
	table := func(name string, age time.Duration) uuid.UUID {
		t.Helper()
		tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: name, Visibility: "workspace"}, owner)
		if err != nil {
			t.Fatal(err)
		}
		f, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Item", Type: model.FieldText}, owner)
		if err != nil {
			t.Fatal(err)
		}
		// A table just made is indexed in the background (indexSoon). Its
		// mark, landing after the one taken away below, made the table whole
		// again in a warm package run, and nothing was tested (V-33): the
		// setup waits for that work before it makes the table one to repair.
		if busy := helpers.SettleBackground(30 * time.Second); len(busy) > 0 {
			t.Fatalf("background work still running 30 s on: %v", busy)
		}
		vals, _ := json.Marshal(map[string]interface{}{f.Id.String(): name + " 1"})
		exec(`INSERT INTO data_table_rows (id, table_id, "values", position, position_key, row_no, created_by, created_at, updated_at)
			VALUES ($1, $2, $3::jsonb, 0, 'a0', 1, $4, NOW(), NOW())`, uuid.New(), tb.Id, string(vals), owner.UserID)
		exec(`UPDATE data_tables SET created_at = NOW() - make_interval(secs => $2::float8), cell_index_at = NULL WHERE id = $1`, tb.Id, age.Seconds())
		return tb.Id
	}
	stuck := table("Stuck", 3*time.Hour)
	next := table("Next", 2*time.Hour)
	last := table("Last", time.Hour)

	// The oldest table's entries are refused, as Postgres refused a long
	// text's: every pass that takes it fails.
	exec(fmt.Sprintf(`CREATE FUNCTION refuse_stuck_entries() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.table_id = '%s'::uuid THEN
				RAISE EXCEPTION 'this table''s entries are refused';
			END IF;
			RETURN NEW;
		END $$`, stuck))
	exec(`CREATE TRIGGER refuse_stuck_entries BEFORE INSERT ON data_table_cell_index FOR EACH ROW EXECUTE FUNCTION refuse_stuck_entries()`)

	log := &heard{}
	was := helpers.Logger
	helpers.Logger = slog.New(log)
	t.Cleanup(func() { helpers.Logger = was })

	whole := func(id uuid.UUID) bool {
		t.Helper()
		ok, _, err := model.CellIndexState(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	failures := func(id uuid.UUID) (int, sql.NullTime) {
		t.Helper()
		var n int
		var at sql.NullTime
		if err := db.QueryRowContext(ctx, `SELECT cell_index_failures, cell_index_retry_at FROM data_tables WHERE id = $1`, id).Scan(&n, &at); err != nil {
			t.Fatal(err)
		}
		return n, at
	}
	// waits is whether a table's next try is wait from start, give or take
	// the quarter more helpers.Backoff adds and a second of clocks.
	waits := func(at sql.NullTime, start time.Time, wait time.Duration) bool {
		return at.Valid && !at.Time.Before(start.Add(wait-time.Second)) && !at.Time.After(time.Now().Add(wait*5/4+time.Second))
	}

	// The setup holds: before the pass, none of the three is whole.
	for _, tb := range []struct {
		name string
		id   uuid.UUID
	}{{"Stuck", stuck}, {"Next", next}, {"Last", last}} {
		if whole(tb.id) {
			t.Fatalf("before the first pass, %s is whole already: the setup made no table to repair", tb.name)
		}
	}

	// One pass: the tables made after the stuck one are indexed.
	start := time.Now()
	passErr := rowQuery.IndexTablesPass(ctx)
	if !whole(next) || !whole(last) {
		t.Errorf("after a pass, the tables made after one the sweep can't index aren't indexed: next %v, last %v (pass: %v)", whole(next), whole(last), passErr)
	}
	if whole(stuck) {
		t.Fatal("the stuck table was indexed: its entries weren't refused, so nothing was tested")
	}
	if passErr != nil {
		t.Errorf("a pass that went past a table it couldn't index answered %v, which the sweep logs at every pass", passErr)
	}
	if n, at := failures(stuck); n != 1 || !waits(at, start, rowQuery.RetryFirst) {
		t.Errorf("after its first failure: %d failures, tried again at %v (from %v)", n, at, start)
	}
	if n := log.naming(stuck.String()); n != 1 {
		t.Errorf("its failure was logged %d times, not once: %q", n, log.said)
	}

	// Before its wait ends, a pass leaves it alone, and says nothing.
	if err := rowQuery.IndexTablesPass(ctx); err != nil {
		t.Errorf("a pass with nothing to take: %v", err)
	}
	if n, _ := failures(stuck); n != 1 {
		t.Errorf("a pass took the stuck table before its wait ended: %d failures", n)
	}
	if n := log.naming(stuck.String()); n != 1 {
		t.Errorf("its failure was logged %d times, not once: %q", n, log.said)
	}

	// Its wait over, a pass takes it again: it fails again and waits twice
	// as long, and nothing more is logged.
	exec(`UPDATE data_tables SET cell_index_retry_at = NOW() - INTERVAL '1 second' WHERE id = $1`, stuck)
	start = time.Now()
	if err := rowQuery.IndexTablesPass(ctx); err != nil {
		t.Errorf("a pass that went past it again: %v", err)
	}
	if n, at := failures(stuck); n != 2 || !waits(at, start, 2*rowQuery.RetryFirst) {
		t.Errorf("after its second failure: %d failures, tried again at %v (from %v)", n, at, start)
	}
	if n := log.naming(stuck.String()); n != 1 {
		t.Errorf("its second failure was logged too: %q", log.said)
	}

	// Mended, it's indexed at its next try, its record is cleared, and that
	// is said once.
	exec(`DROP TRIGGER refuse_stuck_entries ON data_table_cell_index`)
	exec(`UPDATE data_tables SET cell_index_retry_at = NOW() - INTERVAL '1 second' WHERE id = $1`, stuck)
	if err := rowQuery.IndexTablesPass(ctx); err != nil {
		t.Errorf("the pass that indexed it: %v", err)
	}
	if !whole(stuck) {
		t.Error("the mended table wasn't indexed")
	}
	if n, at := failures(stuck); n != 0 || at.Valid {
		t.Errorf("once indexed, its record says %d failures and a try at %v", n, at)
	}
	if n := log.naming(stuck.String()); n != 2 {
		t.Errorf("its mending was said %d times, not once: %q", n-1, log.said)
	}
	if waiting, err := model.TablesWithoutIndex(ctx, 100, rowQuery.IndexVersion); err != nil || len(waiting) != 0 {
		t.Errorf("tables still waiting: %v %v", waiting, err)
	}
}
