package business

// The cell index (tables A3, D-TB-1): each stored cell's value typed for the
// SQL to compare and sort (compile.go), worked out here from the same
// families the evaluator reads, so the index means what the evaluator means.
// It is kept in every write's own transaction (a row ledger observer), and
// made whole for tables that had rows before it by tables.cellindex.repair,
// which marks each table once it is; a table without the mark is answered by
// the evaluator.
//
// What an entry holds: a number's value (n); text, a choice's label and a
// day's date as text (t: trimmed, lower case; a day as 2026-10-08), which
// filters read; a text's or a choice's label's natural sort key (s,
// sortKey.go), which sorts read; a moment (d); a tick, only when ticked (b); a
// list's labels or a person's or an item's ids (a). An empty cell has no
// entry.
//
// A table's mark says which version of the entries made it whole
// (IndexVersion): the repair sweep makes a table of an older version whole
// again, and until then the index answers what that version's entries hold.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	computedStore "github.com/akashc777/OneCamp/business/ComputedStore"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func init() {
	rowLedger.Observers.Register(rowLedger.Observer{Name: "tables.cell_index", Committing: indexChanges})
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: SweepCellIndexRepair, Every: 5 * time.Minute, RunAtStart: true, Run: indexTables})
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: computedStore.SweepRecompute, Every: time.Minute, RunAtStart: true, Run: RecomputeKept})
}

// IndexVersion is the version of the entries IndexTable makes a table's index
// whole with, and marks it by (data_tables.cell_index_version): 1, the values
// (T5); 2, with a text's and a choice's sort key s (T5c).
const IndexVersion = 2

// sortKeysSince is the first version whose entries hold s: a table marked
// older answers a sort on a text or a choice by the evaluator (Run).
const sortKeysSince = 2

// indexed is whether a column has entries: a stored cell of a family the
// index holds.
func indexed(c *column) bool {
	return c.col == colNone && c.family != famRowLink && c.family != famComputed
}

// entryOf is a cell's entry, false for an empty cell.
func entryOf(c *column, raw interface{}) (model.IndexCell, bool) {
	e := model.IndexCell{Field: c.field.Id}
	switch c.family {
	case famText, famChoice:
		if c.family == famChoice {
			if _, isText := raw.(string); !isText {
				return e, false
			}
		}
		text, _ := textOf(raw)
		low := strings.ToLower(strings.TrimSpace(text))
		if low == "" {
			return e, false
		}
		key := naturalKey(text)
		e.T, e.S = &low, &key
		if c.family == famChoice {
			// Its option's place, a label no option has after them all: a
			// sort's first key, its label's key the second (order.go).
			at := optionPlace(c, low)
			e.N = &at
		}
	case famNumber:
		f, ok := raw.(float64)
		if !ok {
			return e, false
		}
		e.N = &f
	case famDate:
		s, ok := raw.(string)
		if !ok {
			return e, false
		}
		if len(s) == len(dayLayout) {
			day, err := time.Parse(dayLayout, s)
			if err != nil {
				return e, false
			}
			// A day is its date (t, which filters read in the reader's
			// zone), and sorts as its midnight at UTC (d), as order.go
			// sorts it, so one index orders days and moments together.
			e.T, e.D = &s, &day
		} else if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			t = t.UTC()
			e.D = &t
		} else {
			return e, false
		}
	case famCheckbox:
		if b, _ := raw.(bool); !b {
			return e, false
		}
		e.B = true
	case famMulti:
		labels := labelsOf(raw)
		if len(labels) == 0 {
			return e, false
		}
		for i, l := range labels {
			labels[i] = strings.ToLower(strings.TrimSpace(l))
		}
		e.A = labels
	case famPeople, famRefs:
		ids := idsOf(c, &evalRow{values: map[string]interface{}{c.key: raw}})
		if len(ids) == 0 {
			return e, false
		}
		e.A = ids
	default:
		return e, false
	}
	return e, true
}

// walkedColumn is whether a column's cells are walked in a sort (walks.go): a
// text, a number, a choice, a date or a tick. Such a column has an entry for
// every live row, an empty one (no value in any of n, t, d, b, a) for a cell
// with none, so the rows with no value are walked along an index too.
func walkedColumn(c *column) bool {
	switch c.family {
	case famText, famNumber, famChoice, famDate, famCheckbox:
		return true
	}
	return false
}

// cellEntry is a cell's entry as the index keeps it: entryOf's, or an empty
// one for a walked column's empty cell; false for no entry at all.
func cellEntry(c *column, raw interface{}) (model.IndexCell, bool) {
	if e, ok := entryOf(c, raw); ok {
		return e, true
	}
	return model.IndexCell{Field: c.field.Id}, walkedColumn(c)
}

// entriesOf is a row's entries, for the columns given.
func entriesOf(cols []*column, r *model.Row) []model.IndexCell {
	var values map[string]interface{}
	_ = json.Unmarshal([]byte(r.Values), &values)
	var out []model.IndexCell
	for _, c := range cols {
		if e, ok := cellEntry(c, values[c.key]); ok {
			e.Table, e.Row = r.TableId, r.Id
			if r.PositionKey != "" {
				e.P = &r.PositionKey
			}
			out = append(out, e)
		}
	}
	return out
}

// indexedColumns is a table's columns the index holds, by field id.
func indexedColumns(fields []*model.Field) map[string]*column {
	out := map[string]*column{}
	for _, f := range fields {
		if c := columnOf(f); indexed(c) {
			out[c.key] = c
		}
	}
	return out
}

func listOf(cols map[string]*column, only []string) []*column {
	var out []*column
	if only == nil {
		for _, c := range cols {
			out = append(out, c)
		}
		return out
	}
	for _, id := range only {
		if c := cols[id]; c != nil {
			out = append(out, c)
		}
	}
	return out
}

// indexChanges keeps the index in a write's transaction: a cell from its new
// value, a row made or brought back from what's stored, and every row's cell
// of a field whose type or options changed. Every statement below runs in tx,
// which ctx carries from here on.
func indexChanges(ctx context.Context, tx *sql.Tx, cs *rowLedger.ChangeSet) error {
	ctx = pgexec.WithTx(ctx, tx)
	fields, err := model.LiveFields(ctx, cs.Table)
	if err != nil {
		return err
	}
	cols := indexedColumns(fields)
	if err := keptChanges(ctx, cs); err != nil {
		return err
	}
	if cs.Kind == rowLedger.ChangeField {
		return reindexFields(ctx, cs.Table, listOf(cols, cs.Fields))
	}
	wholeRows := map[uuid.UUID]bool{}
	var moved []uuid.UUID
	var cellRows, cellFields []uuid.UUID
	var entries []model.IndexCell
	for _, ch := range cs.Changes {
		switch {
		case ch.Field == rowLedger.WholeRow:
			if len(ch.After) > 0 && string(ch.After) != "null" {
				wholeRows[ch.Row] = true
			}
		case ch.Field == rowLedger.FieldPosition:
			moved = append(moved, ch.Row) // its entries' place follows it
		case strings.HasPrefix(ch.Field, "#"):
			// A link change: no cell changed.
		default:
			c := cols[ch.Field]
			if c == nil || wholeRows[ch.Row] {
				continue
			}
			cellRows = append(cellRows, ch.Row)
			cellFields = append(cellFields, c.field.Id)
			var raw interface{}
			_ = json.Unmarshal(ch.After, &raw)
			if e, ok := cellEntry(c, raw); ok {
				e.Table, e.Row = cs.Table, ch.Row
				entries = append(entries, e)
			}
		}
	}
	// The cells that changed, each in place of its own entry, all at once:
	// their rows' other entries stay.
	if err := model.ReplaceCells(ctx, cellRows, cellFields, entries); err != nil {
		return err
	}
	if err := model.SyncPlaces(ctx, moved); err != nil {
		return err
	}
	if len(wholeRows) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(wholeRows))
	for id := range wholeRows {
		ids = append(ids, id)
	}
	return reindexRows(ctx, cs.Table, ids, listOf(cols, nil), nil)
}

// reindexRows rewrites rows' entries for cols (every field's when only is
// nil), from what is stored, in the transaction ctx carries.
func reindexRows(ctx context.Context, table uuid.UUID, ids []uuid.UUID, cols []*column, only []uuid.UUID) error {
	rows, err := model.RowsForIndex(ctx, table, ids)
	if err != nil {
		return err
	}
	var entries []model.IndexCell
	for _, r := range rows {
		entries = append(entries, entriesOf(cols, r)...)
	}
	return model.ReplaceCellIndex(ctx, ids, only, entries)
}

// reindexBatch is how many rows one pass reads at a time.
const reindexBatch = 500

// reindexFields rewrites every live row's entries of fields, in the
// transaction ctx carries: a type changed, or a choice's options.
func reindexFields(ctx context.Context, table uuid.UUID, cols []*column) error {
	if len(cols) == 0 {
		return nil
	}
	only := make([]uuid.UUID, len(cols))
	for i, c := range cols {
		only[i] = c.field.Id
	}
	after := uuid.Nil
	for {
		ids, err := model.RowIDsAfter(ctx, table, after, reindexBatch)
		if err != nil || len(ids) == 0 {
			return err
		}
		if err := reindexRows(ctx, table, ids, cols, only); err != nil {
			return err
		}
		after = ids[len(ids)-1]
	}
}

// inBatch runs fn in a transaction of its own, which the ctx fn is given
// carries to its statements: one batch of a table's rows indexed, bounded by
// BulkTimeout (pgexec.InTx's TxTimeout is shorter).
func inBatch(ctx context.Context, fn func(ctx context.Context) error) error {
	return postgresInit.InTx(ctx, postgresInit.BulkTimeout, func(ctx context.Context, tx *sql.Tx) error {
		return fn(pgexec.WithTx(ctx, tx))
	})
}

// batchIndexed runs after each batch IndexTable commits. A seam for tests: a
// write that lands while a table is walked.
var batchIndexed = func(context.Context, uuid.UUID) {}

// IndexTable makes a table's cell index whole, a batch of rows to a
// transaction (inBatch), and marks it with IndexVersion. A write while it
// runs indexes its own rows. A table whose entries are an older version's is
// made whole again the same way: until it's marked, Run answers only what
// the older entries hold. A table whose mark is taken away while it's walked
// (MarkCellIndexStale: a field whose entries couldn't be written for the rows
// already walked) isn't marked; the repair sweep's next pass walks it again.
func IndexTable(ctx context.Context, table uuid.UUID) error {
	gen, err := model.CellIndexStaleGen(ctx, table)
	if err != nil {
		return err
	}
	// The Computed Store's formulas and rollups are filled in the same pass:
	// a whole row's entries are replaced, theirs included.
	kept, err := startKeeping(ctx, table, nil)
	if err != nil {
		return err
	}
	after := uuid.Nil
	indexed := 0
	for {
		ids, err := model.RowIDsAfter(ctx, table, after, reindexBatch)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		// A kept batch (inKeptBatch): it replaces the rows' kept entries too.
		if _, err := inKeptBatch(ctx, table, false, func(ctx context.Context) error {
			// The fields as they are now: one may have changed since.
			fs, err := model.LiveFields(ctx, table)
			if err != nil {
				return err
			}
			if err := reindexRows(ctx, table, ids, listOf(indexedColumns(fs), nil), nil); err != nil {
				return err
			}
			return kept.fill(ctx, ids)
		}); err != nil {
			return err
		}
		after = ids[len(ids)-1]
		indexed += len(ids)
		batchIndexed(ctx, table)
	}
	marked, failed, err := model.MarkCellIndexed(ctx, table, IndexVersion, gen)
	if err != nil {
		return err
	}
	if err := kept.done(ctx); err != nil {
		return err
	}
	if !marked {
		helpers.LogInfoWithContext(ctx, "business/RowQuery: table %s's cell index mark was taken away while it was walked, so the repair sweep's next pass walks it again", table)
	} else if failed > 0 {
		// Its failure was logged once, at the first pass it failed.
		helpers.LogInfoWithContext(ctx, "business/RowQuery: table %s's cell index is whole, after %d passes of the repair sweep couldn't make it so", table, failed)
	}
	return CountAfterFill(ctx, indexed)
}

// CountAfterFill has the planner count the rows and the cell index again
// (model.AnalyzeRows) once rows were written or indexed at once, at least
// analyzeAfter of them: a walk's, or an import's into a table whose index
// kept up with it, which no walk follows (V-48). Many rows the planner hasn't
// counted until the autovacuum's next look made it plan badly: taking the
// table for one row, it joined a filter's entries by a loop over them all,
// and a window of 10,000 rows ran past the database's timeout; a first
// window's summaries and groups ran past their budget and were left out.
//
// A count that fails (a concurrent ANALYZE or the autovacuum's can hold it
// past its timeout) leaves the planner to the autovacuum's next look, and
// never fails the fill that asked: its entries are whole. AnalyzeRows logs
// it. Only a fill that was stopped hears the error (V-107).
//
// The counts run one at a time (counts): fills that ask together, as fields
// made at once do, share the next count after their entries, and never each
// start their own to queue on ANALYZE's lock (V-108).
func CountAfterFill(ctx context.Context, rows int) error {
	if rows < analyzeAfter {
		return nil
	}
	if err := counts.Do(ctx); err != nil && ctx.Err() != nil {
		return err
	}
	return nil
}

// counts is the planner's counts, one at a time (CountAfterFill).
var counts = helpers.NewCoalesced("RowQuery.CountAfterFill", func(ctx context.Context) error {
	return analyzeRows(ctx)
})

// analyzeRows is model.AnalyzeRows (a test fails it: CountAfterFill).
var analyzeRows = model.AnalyzeRows

// analyzeAfter is how many rows written or indexed at once have the planner
// count the tables again (CountAfterFill).
const analyzeAfter = 1000

// IndexField makes a field's entries whole for every row of its table, a
// batch of rows to a transaction (inBatch): a field made (or brought back
// from the trash) after its table's rows, which every walked column has an
// entry of (cellEntry). A write while it runs indexes its own rows. A field
// the index doesn't hold (a formula or a rollup: the store fills those), or
// no longer holds once it's deleted or its type changes, has nothing more to
// walk, and its rows aren't counted for the planner (CR-97).
func IndexField(ctx context.Context, table, field uuid.UUID) error {
	after := uuid.Nil
	indexed := 0
	for {
		ids, err := model.RowIDsAfter(ctx, table, after, reindexBatch)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		held := false
		if err := inBatch(ctx, func(ctx context.Context) error {
			fs, err := model.LiveFields(ctx, table)
			if err != nil {
				return err
			}
			c := indexedColumns(fs)[field.String()]
			if held = c != nil; !held {
				return nil
			}
			return reindexRows(ctx, table, ids, []*column{c}, []uuid.UUID{field})
		}); err != nil {
			return err
		}
		if !held {
			break
		}
		after = ids[len(ids)-1]
		indexed += len(ids)
	}
	// The field's new entries (an empty one a row), counted for the planner
	// as a table's are after its fill (V-107): beside the request that made
	// or brought back the field, which never waits on it, as indexSoon counts
	// an import's.
	if indexed >= analyzeAfter {
		helpers.GoSafeNamed("RowQuery.IndexField.count", func() {
			_ = CountAfterFill(context.WithoutCancel(ctx), indexed)
		})
	}
	return nil
}

// IndexOptions makes a choice field's entries' option places (n) its options'
// again, in a transaction of its own: its options were reordered, added or
// taken away by a save that changed no cell, which the ledger's observer
// never hears (a save that changes cells reindexes the field in its own
// transaction). Only entries whose place changed are written. Not a choice:
// nothing to do.
func IndexOptions(ctx context.Context, table, field uuid.UUID) error {
	return inBatch(ctx, func(ctx context.Context) error {
		fs, err := model.LiveFields(ctx, table)
		if err != nil {
			return err
		}
		c := indexedColumns(fs)[field.String()]
		if c == nil || c.family != famChoice {
			return nil
		}
		labels := make([]string, len(c.options))
		for i, o := range c.options {
			labels[i] = strings.ToLower(strings.TrimSpace(o))
		}
		// optionPlace's places: the first option a label is, from 1; a label
		// none is, after them all.
		return model.PlaceChoices(ctx, field, labels, float64(len(labels)+1))
	})
}

// passBudget is how long one pass of tables.cellindex.repair indexes for.
const passBudget = 2 * time.Minute

// retryFirst and retryMost bound how long the repair sweep leaves a table it
// couldn't index before a pass takes it again: one pass's interval after the
// first failure, twice as long after each failure after it, up to a day.
const (
	retryFirst = 5 * time.Minute
	retryMost  = 24 * time.Hour
)

// retryWait is how long a table waits after its nth failure in a row.
func retryWait(failures int) time.Duration {
	return helpers.Backoff(failures, retryFirst, retryMost)
}

// indexTables makes whole the cell index of tables without one, or with an
// older version's, oldest first, for passBudget.
//
// A table that can't be indexed is recorded as failing (MarkCellIndexFailed)
// and left until its wait ends, and the pass goes on to the next table. Each
// pass used to take that table first and stop there, so one table held up
// every table made after it, and its error was logged every pass; now it's
// logged at the first failure, and once more when the table is indexed.
func indexTables(ctx context.Context) error {
	until := time.Now().Add(passBudget)
	for time.Now().Before(until) {
		tables, err := model.TablesWithoutIndex(ctx, 10, IndexVersion)
		if err != nil || len(tables) == 0 {
			return err
		}
		for _, t := range tables {
			err := IndexTable(ctx, t)
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err() // the server is stopping: not the table's failure
			}
			n, recordErr := model.MarkCellIndexFailed(ctx, t, retryWait)
			if recordErr != nil {
				// Unrecorded, it would be this pass's next table again.
				return fmt.Errorf("index table %s: %v; its failure wasn't recorded: %w", t, err, recordErr)
			}
			if n == 1 {
				helpers.LogErrorWithContext(ctx, "business/RowQuery: table %s's cell index couldn't be made whole, so its rows are answered without it: %v. The repair sweep tries it again later, less often each time, and says so here once", t, err)
			}
		}
	}
	return nil
}
