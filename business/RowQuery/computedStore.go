package business

// The Computed Store's entries (airtable-03): a formula's or a rollup's values
// kept in the cell index, typed by what the field gives, so a view sorts,
// filters, groups and adds up by them along the index at 100,000 rows, as it
// does by a stored field.
//
// Who may be answered by a kept value is business/DataTable's gate
// (computedStore.Values.Serves): only a reader who opens every table the
// value reaches, at every level, for a value that is the same whoever reads
// it and whenever. Every other reader is answered by their own read, as
// before: worked out as them, refused past MaxComputedScan rows. Whether there
// is a current value to answer with is this file's: the field's entries were
// filled at its generation and no more than keptInline of its rows wait to be
// worked out again (dirty marks, made in each row write's own transaction),
// which a read works out first. The generation is raised in the transaction
// of any write to a field it reads, at any level, or to the primary field of
// a table it reaches (migration 538's trigger, whatever path writes the
// field), and every formula and rollup is held from the moment it exists, so
// the sweep fills it. A stale value never answers anyone.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	computedStore "github.com/akashc777/OneCamp/business/ComputedStore"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// keptInline is how many rows waiting to be worked out again a read works
// out before it answers by kept values; past it the read is answered as the
// reader's own (runWorked), and the sweep catches up.
const keptInline = 2000

// computedRead is the formula and rollup columns a spec filters, sorts,
// groups or adds up by, each once.
func computedRead(spec query.Spec, cols map[string]*column) []*column {
	var out []*column
	seen := map[string]bool{}
	note := func(ref string) {
		if c := cols[ref]; c != nil && c.family == famComputed && !seen[c.key] {
			seen[c.key] = true
			out = append(out, c)
		}
	}
	var walk func(f *query.Filter)
	walk = func(f *query.Filter) {
		if f == nil {
			return
		}
		note(f.Field)
		for i := range f.Filters {
			walk(&f.Filters[i])
		}
	}
	walk(spec.Filter)
	for _, s := range spec.Sorts {
		note(s.Field)
	}
	for _, s := range spec.Summaries {
		note(s.Field)
	}
	for _, g := range spec.Groups {
		note(g.Field)
	}
	return out
}

// keptCols is cols with each of read answered from the store, comparing as
// what it gives, or nil: who is answered by their own read. Nil unless the
// gate serves who by every one of them, each holds entries filled at its
// current generation, as the kind the gate says, and at most keptInline of
// their rows wait, which are worked out first. Any error is a nil.
func keptCols(ctx context.Context, who tableAccess.Who, table uuid.UUID, read []*column, cols map[string]*column) map[string]*column {
	ids := make([]string, len(read))
	fields := make([]uuid.UUID, len(read))
	for i, c := range read {
		ids[i], fields[i] = c.key, c.field.Id
	}
	kinds, err := computedStore.Values.Serves(ctx, who, table, ids)
	if err != nil || kinds == nil {
		return nil
	}
	states, err := model.ComputedStates(ctx, fields)
	if err != nil {
		return nil
	}
	for i, id := range fields {
		if s, ok := states[id]; !ok || !s.Current() || s.Kind != kinds[ids[i]] {
			return nil
		}
	}
	if done, err := workDirty(ctx, table, fields, keptInline); err != nil || !done {
		return nil
	}
	out := make(map[string]*column, len(cols))
	for k, c := range cols {
		out[k] = c
	}
	for _, c := range read {
		kept := *c
		kept.family = kindOfResult(kinds[c.key])
		out[c.key], out["field:"+c.key] = &kept, &kept
	}
	return out
}

// workDirty is a reader's work on the rows waiting among fields' marks,
// when at most limit marks wait: it works them out again and answers exactly
// the marks it read; done is whether it answered every mark waiting when it
// read them (a mark from a write that commits after the read stays, for the
// next pass). It reads the marks again in a kept batch as a reader
// (inKeptBatch): it waits for the table's kept lock no longer than about a
// second, so a sweep or fill batch that commits in that time leaves the
// marks to it; while a longer one holds the lock it isn't done, and the
// reader answers by its own read.
func workDirty(ctx context.Context, table uuid.UUID, fields []uuid.UUID, limit int) (bool, error) {
	if _, marks, err := model.DirtyComputed(ctx, fields, limit+1); err != nil || len(marks) == 0 || len(marks) > limit {
		return err == nil && len(marks) == 0, err
	}
	done := false
	_, err := inKeptBatch(ctx, table, true, func(ctx context.Context) error {
		rows, marks, err := model.DirtyComputed(ctx, fields, limit+1)
		if err != nil || len(marks) > limit {
			return err
		}
		if len(marks) > 0 {
			if _, err := keep(ctx, table, rows, nil); err != nil {
				return err
			}
			if err := model.AnswerDirty(ctx, marks); err != nil {
				return err
			}
		}
		done = true
		return nil
	})
	return done && err == nil, err
}

// inKeptBatch is inBatch for a batch that writes table's kept entries: its
// first statement takes the table's kept lock (model.LockKept), held to its
// commit, so a table's kept batches run one at a time, and what fn reads,
// marks and rows, is at least as new as anything a batch before it wrote.
// Row writes never take the lock. A reader waits for it no longer than
// model.LockKept lets one; then fn doesn't run (ran is false).
func inKeptBatch(ctx context.Context, table uuid.UUID, reader bool, fn func(ctx context.Context) error) (ran bool, err error) {
	err = inBatch(ctx, func(ctx context.Context) error {
		locked, err := model.LockKept(ctx, table, reader)
		if err != nil || !locked {
			return err
		}
		ran = true
		return fn(ctx)
	})
	return ran, err
}

// keep works out and writes the kept entries of table's formulas and rollups
// for rows ids: each kept field's entries on those rows are replaced (only
// the ones only names, when it isn't nil), other fields' left alone. It runs
// in a kept batch (inKeptBatch).
func keep(ctx context.Context, table uuid.UUID, ids []uuid.UUID, only map[string]bool) (*computedStore.Worked, error) {
	w, err := computedStore.Values.Work(ctx, table, ids)
	if err != nil || len(w.Kinds) == 0 || len(ids) == 0 {
		return w, err
	}
	fields := make([]uuid.UUID, 0, len(w.Kinds))
	cols := make([]*column, 0, len(w.Kinds))
	for id, kind := range w.Kinds {
		f, err := uuid.Parse(id)
		if err != nil || (only != nil && !only[id]) {
			continue
		}
		fields = append(fields, f)
		cols = append(cols, &column{field: &model.Field{Id: f, TableId: table}, key: id, family: kindOfResult(kind)})
	}
	if len(fields) == 0 {
		// None of them is kept: no fields would replace every field's
		// entries on the rows (model.ReplaceCellIndex).
		return w, nil
	}
	var entries []model.IndexCell
	for _, r := range w.Rows {
		entries = append(entries, entriesOf(cols, r)...)
	}
	keptWorked(ctx, table)
	return w, model.ReplaceCellIndex(ctx, ids, fields, entries)
}

// keptWorked runs in keep once the rows are worked out, before their kept
// entries are written. A seam for tests: a pass or a write that lands
// between a batch's reads and its writes.
var keptWorked = func(context.Context, uuid.UUID) {}

// keeping is one whole fill of a table's kept fields: each one's generation
// and the highest dirty mark when it started, and what Work said of them.
// only is the fields it fills, by id, when it fills some of them (the ones
// that aren't current, V-108); nil is every one (IndexTable's whole pass).
type keeping struct {
	table uuid.UUID
	gens  map[uuid.UUID]int64
	mark  int64
	w     *computedStore.Worked
	only  map[string]bool
	// fields are gens' fields, whose progress the fill counts (airtable-03b);
	// started and rows time it, for the log.
	fields  []uuid.UUID
	started time.Time
	rows    int
}

// startKeeping reads where a whole fill of the table's formulas and rollups
// starts from: each one's generation and the highest dirty mark, and only
// then what each gives and reaches as the settings are now, so a change to
// those settings after it raises a generation the fill's mark won't match.
// The tables the fill will reach join each field's reach before any row is
// worked out, so a write in one of them meanwhile is marked; a write marking
// in the table or one it reaches, not yet committed, holds the fill off
// (model.ReachComputed's lock), and then it is nil, for the sweep's next
// pass. fields is the ones it fills; nil is every one the table holds.
func startKeeping(ctx context.Context, table uuid.UUID, fields []uuid.UUID) (*keeping, error) {
	k := &keeping{table: table}
	held, err := fields, error(nil)
	if fields == nil {
		held, err = model.StoredFieldsOf(ctx, table)
	} else {
		k.only = make(map[string]bool, len(fields))
		for _, f := range fields {
			k.only[f.String()] = true
		}
	}
	if err != nil || len(held) == 0 {
		return k, err
	}
	if k.gens, k.mark, err = model.ComputedFillStart(ctx, held); err != nil {
		return nil, err
	}
	if k.w, err = computedStore.Values.Work(ctx, table, nil); err != nil {
		return nil, err
	}
	reaches := make(map[uuid.UUID][]uuid.UUID, len(k.w.Reaches))
	for id, tables := range k.w.Reaches {
		if f, err := uuid.Parse(id); err == nil && (k.only == nil || k.only[id]) {
			reaches[f] = tables
		}
	}
	if ok, err := model.ReachComputed(ctx, table, reaches); err != nil || !ok {
		return nil, err
	}
	// How far the fill has got, for a read past what one read works out
	// meanwhile (StillWorkingOut).
	for f := range k.gens {
		k.fields = append(k.fields, f)
	}
	total, err := model.CountRows(ctx, table)
	if err != nil {
		return nil, err
	}
	if err := model.StartFillProgress(ctx, k.fields, total); err != nil {
		return nil, err
	}
	k.started = time.Now()
	return k, nil
}

// fill works out and writes the kept entries of rows ids. A nil keeping (a
// write held the fill off) keeps nothing: the sweep fills those fields.
func (k *keeping) fill(ctx context.Context, ids []uuid.UUID) error {
	if k == nil || len(k.gens) == 0 {
		return nil
	}
	start := time.Now()
	if _, err := keep(ctx, k.table, ids, k.only); err != nil {
		return err
	}
	k.rows += len(ids)
	took := time.Since(start)
	helpers.LogInfoWithContext(ctx, "business/RowQuery: table %s's kept fill: %d rows in %d ms (%.0f rows/s), %d so far", k.table, len(ids), took.Milliseconds(), float64(len(ids))/took.Seconds(), k.rows)
	return model.AddFillProgress(ctx, k.fields, len(ids))
}

// done marks every field filled at the generation it started from, as what
// it gives ("" for a field the store can't keep, which each read works out),
// in one statement: a field whose generation moved on meanwhile is left for
// the sweep's next fill.
func (k *keeping) done(ctx context.Context) error {
	if k == nil {
		return nil
	}
	if k.rows > 0 {
		took := time.Since(k.started)
		helpers.LogInfoWithContext(ctx, "business/RowQuery: table %s's kept fill done: %d rows of %d fields in %d ms (%.0f rows/s)", k.table, k.rows, len(k.fields), took.Milliseconds(), float64(k.rows)/took.Seconds())
	}
	fills := make([]model.ComputedFill, 0, len(k.gens))
	for f, gen := range k.gens {
		id := f.String()
		fills = append(fills, model.ComputedFill{Field: f, Gen: gen, Kind: k.w.Kinds[id], Reaches: k.w.Reaches[id]})
	}
	_, err := model.MarkComputedFilled(ctx, fills, k.mark)
	return err
}

// fillKept fills table's kept fields that aren't current (fields) again over
// every row, a batch of rows at a time, and never the ones that are: a field
// made or changed fills itself and the fields that read it (migration 538
// raises only those), not every kept field of its table (V-108). A table
// whose index isn't whole is IndexTable's to fill.
func fillKept(ctx context.Context, table uuid.UUID, fields []uuid.UUID) error {
	whole, _, err := model.CellIndexState(ctx, table)
	if err != nil || !whole {
		return err
	}
	k, err := startKeeping(ctx, table, fields)
	if err != nil || k == nil || len(k.gens) == 0 {
		return err
	}
	after := uuid.Nil
	filled := 0
	for {
		ids, err := model.RowIDsAfter(ctx, table, after, reindexBatch)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		if _, err := inKeptBatch(ctx, table, false, func(ctx context.Context) error { return k.fill(ctx, ids) }); err != nil {
			return err
		}
		after = ids[len(ids)-1]
		filled += len(ids)
	}
	// Counted for the planner before the fields answer from the store (V-107):
	// a formula made after a big import wrote an entry a row the planner
	// hadn't seen, so its first window and sums planned for a handful and ran
	// past the statement timeout. The sweep's own pass, so it waits for it.
	if err := CountAfterFill(ctx, filled); err != nil {
		return err
	}
	return k.done(ctx)
}

// keptChanges is the store's part of a row write, in its own transaction:
// rows written, made, deleted or linked mark the kept values they change
// (model.MarkComputedDirty, which holds their tables against a fill
// recording what it reaches until the write commits). A field's change, cells rewritten with it, raised
// the generation of every kept field it can change in this same transaction
// (migration 538's trigger: the ones that read it), so those are filled
// again whole.
func keptChanges(ctx context.Context, cs *rowLedger.ChangeSet) error {
	if cs.Kind == rowLedger.ChangeField {
		return nil
	}
	seen := map[uuid.UUID]bool{}
	var rows []uuid.UUID
	add := func(id uuid.UUID) {
		if id != uuid.Nil && !seen[id] {
			seen[id] = true
			rows = append(rows, id)
		}
	}
	for _, ch := range cs.Changes {
		if ch.Field == rowLedger.FieldPosition {
			continue // a move changes no value
		}
		add(ch.Row)
		if strings.HasPrefix(ch.Field, rowLedger.LinksPrefix) {
			var links struct {
				Add, Remove []struct{ From, To uuid.UUID }
			}
			_ = json.Unmarshal(ch.After, &links)
			for _, l := range append(links.Add, links.Remove...) {
				add(l.From)
				add(l.To)
			}
		}
	}
	return model.MarkComputedDirty(ctx, []uuid.UUID{cs.Table}, rows)
}

// RecomputeKept is the sweep (computedStore.SweepRecompute; exported for a
// test): it fills again
// the kept fields whose entries aren't current, then works out again the
// rows waiting, a batch at a time, within its pass budget.
func RecomputeKept(ctx context.Context) error {
	until := time.Now().Add(passBudget)
	unfilled, err := model.UnfilledComputed(ctx, 20)
	if err != nil {
		return err
	}
	// Each table's fields that aren't current, filled together: the ones that
	// are stay as they are.
	tables := map[uuid.UUID][]uuid.UUID{}
	for f, t := range unfilled {
		tables[t] = append(tables[t], f)
	}
	for t, fields := range tables {
		if time.Now().After(until) {
			return nil
		}
		if err := fillKept(ctx, t, fields); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			helpers.LogErrorWithContext(ctx, "business/RowQuery: table %s's kept formulas and rollups couldn't be filled, so its readers are answered by their own reads: %v", t, err)
		}
	}
	for time.Now().Before(until) {
		dirty, err := model.DirtyTables(ctx, 10)
		if err != nil || len(dirty) == 0 {
			return err
		}
		for _, t := range dirty {
			fields, err := model.StoredFieldsOf(ctx, t)
			if err != nil {
				return err
			}
			if _, err := inKeptBatch(ctx, t, false, func(ctx context.Context) error {
				rows, marks, err := model.DirtyComputed(ctx, fields, keptInline)
				if err != nil || len(marks) == 0 {
					return err
				}
				if _, err := keep(ctx, t, rows, nil); err != nil {
					return err
				}
				return model.AnswerDirty(ctx, marks)
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
