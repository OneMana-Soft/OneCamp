package models

import (
	"context"
	"errors"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// The Computed Store's own state (airtable-03, migration 535): which formula
// and rollup fields keep their values in data_table_cell_index, whether those
// entries are current, and which rows wait to be worked out again. Whether a
// stored value may answer a reader is business/DataTable's call (its gate);
// this is only whether there is a current value to answer with.
//
// Every formula and rollup is held from the moment it exists, and every write
// to a field raises the generations it can change, in its own transaction:
// migration 535's trigger on data_table_fields (538's since V-108: only the
// fields that read the field written), whatever path writes the field.
// Nothing here holds a field or raises a generation for a field write.

// ComputedState is a stored field's: its generation, the one its entries
// were filled at (current when they're equal), and the kind of value its
// entries hold ("number", "text", "checkbox"; "" when the fill found it
// can't be kept, so each read works it out), and how far a whole fill under
// way has got (FillDone of FillTotal rows; migration 537).
type ComputedState struct {
	Gen, FilledGen      int64
	Kind                string
	FillDone, FillTotal int64
}

// Current is whether the field's entries hold its values as of its settings
// and every table it reaches now, dirty rows aside.
func (s ComputedState) Current() bool { return s.FilledGen > 0 && s.FilledGen == s.Gen }

// ComputedStates is the state of each of fields the store holds, by id; a
// field it doesn't hold is missing.
func ComputedStates(ctx context.Context, fields []uuid.UUID) (map[uuid.UUID]ComputedState, error) {
	type row struct {
		id uuid.UUID
		s  ComputedState
	}
	rows, err := pgexec.Query(ctx, `SELECT field_id, gen, filled_gen, kind, fill_done, fill_total FROM data_table_computed WHERE field_id = ANY($1)`,
		func(r pgexec.Row) (x row, err error) {
			err = r.Scan(&x.id, &x.s.Gen, &x.s.FilledGen, &x.s.Kind, &x.s.FillDone, &x.s.FillTotal)
			return x, err
		}, fields)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]ComputedState, len(rows))
	for _, x := range rows {
		out[x.id] = x.s
	}
	return out, nil
}

// StartFillProgress records that a whole fill of fields starts over total
// rows, none of them done yet (airtable-03b).
func StartFillProgress(ctx context.Context, fields []uuid.UUID, total int) error {
	_, err := pgexec.Exec(ctx, `UPDATE data_table_computed SET fill_done = 0, fill_total = $2 WHERE field_id = ANY($1)`, fields, total)
	return err
}

// AddFillProgress counts n more rows of fields' whole fill done, in the batch
// that worked them out, so it commits with their entries.
func AddFillProgress(ctx context.Context, fields []uuid.UUID, n int) error {
	_, err := pgexec.Exec(ctx, `UPDATE data_table_computed SET fill_done = LEAST(fill_done + $2, fill_total) WHERE field_id = ANY($1)`, fields, n)
	return err
}

// computedLock is the key of table l.t's lock between the store's fills and
// row writes, held to the end of a transaction: shared by a row write while
// it marks (MarkComputedDirty), exclusive by a fill while it records what it
// reaches (ReachComputed).
const computedLock = `hashtextextended('onecamp.tables.computed:' || l.t::text, 0)`

// ReachComputed adds to each field's reach the tables a fill of table
// starting now will reach, before it works out any row, in one statement: a
// write in one of them from here on marks the rows whose values it changes
// (MarkComputedDirty), so a row the fill worked out before that write is
// worked out again after it. The fill's mark sets each reach to exactly its
// own (MarkComputedFilled).
//
// It records them in a transaction holding table and every table reached
// exclusively (computedLock), taken without waiting, so a write that marked
// before the reach was there has committed before the fill reads a row, and
// one marking after sees it. A write holding one of them is marking and
// hasn't committed: then nothing is recorded (false) and the fill waits for
// the sweep's next pass. It never queues for the lock, since a queued
// exclusive request would hold up every write behind it, and the reach waits
// no more than computedLockWait for a row.
func ReachComputed(ctx context.Context, table uuid.UUID, reaches map[uuid.UUID][]uuid.UUID) (bool, error) {
	fields, tables := reachPairs(reaches)
	locked := false
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, `SET LOCAL lock_timeout = '`+computedLockWait+`'`); err != nil {
			return err
		}
		ok, err := pgexec.Scalar[bool](ctx, `SELECT bool_and(pg_try_advisory_xact_lock(`+computedLock+`))
			FROM (SELECT DISTINCT t FROM unnest($1::uuid[] || $2::uuid) AS t) AS l(t)`, tables, table)
		if err != nil || !ok {
			return err
		}
		locked = true
		if len(fields) == 0 {
			return nil
		}
		_, err = pgexec.Exec(ctx, `UPDATE data_table_computed c
			SET reaches = ARRAY(SELECT DISTINCT t FROM unnest(c.reaches || n.adds) AS t)
			FROM (SELECT p.f, array_agg(DISTINCT p.t) AS adds FROM unnest($1::uuid[], $2::uuid[]) AS p(f, t) GROUP BY p.f) n
			WHERE c.field_id = n.f AND NOT c.reaches @> n.adds`, fields, tables)
		return err
	})
	return locked && err == nil, err
}

// keptLock is the key of table $1's kept lock, held to the end of a
// transaction by each batch that writes the table's kept entries
// (LockKept). Row writes never take it: theirs is computedLock.
const keptLock = `hashtextextended('onecamp.tables.kept:' || $1::text, 0)`

// LockKept takes table's kept lock for the transaction ctx carries, to its
// commit, before the batch reads a mark or a row: the batches that write a
// table's kept entries run one at a time, so none writes over entries that
// a batch after it worked out. A reader (reader true) waits for the lock no
// longer than keptReaderWait, and is false when another batch still holds it
// then: long enough for a sweep or fill batch to commit, so a reader sorting
// while rows change answers the marks itself (the live walk at 100k refused
// 23 of 80 such sorts when a reader only tried the lock), and short enough
// that no read queues behind a long batch.
func LockKept(ctx context.Context, table uuid.UUID, reader bool) (bool, error) {
	if !reader {
		_, err := pgexec.Exec(ctx, `SELECT pg_advisory_xact_lock(`+keptLock+`)`, table)
		return err == nil, err
	}
	// The wait runs in a savepoint with its own lock_timeout: a wait that runs
	// out (55P03) is rolled back to it and leaves the transaction usable, and
	// one that takes the lock puts the transaction's lock_timeout back.
	timeout, err := pgexec.Scalar[string](ctx, `SELECT current_setting('lock_timeout')`)
	if err != nil {
		return false, err
	}
	if _, err := pgexec.Exec(ctx, `SAVEPOINT kept_wait`); err != nil {
		return false, err
	}
	if _, err := pgexec.Exec(ctx, `SET LOCAL lock_timeout = '`+keptReaderWait+`'`); err != nil {
		return false, err
	}
	if _, err := pgexec.Exec(ctx, `SELECT pg_advisory_xact_lock(`+keptLock+`)`, table); err != nil {
		if !lockNotAvailable(err) {
			return false, err
		}
		_, err = pgexec.Exec(ctx, `ROLLBACK TO SAVEPOINT kept_wait`)
		return false, err
	}
	if _, err := pgexec.Exec(ctx, `RELEASE SAVEPOINT kept_wait`); err != nil {
		return false, err
	}
	_, err = pgexec.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, timeout)
	return err == nil, err
}

// keptReaderWait is the most a reader waits for a table's kept lock.
const keptReaderWait = "1s"

// lockNotAvailable recognises Postgres error 55P03 (lock_timeout ran out)
// from either driver: pgx reports it through SQLState(), lib/pq through Code.
func lockNotAvailable(err error) bool {
	var withState interface{ SQLState() string }
	if errors.As(err, &withState) {
		return withState.SQLState() == "55P03"
	}
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "55P03"
}

// computedLockWait is the most a fill holding its tables' locks waits for
// anything else: the writes it holds up wait no longer than that.
const computedLockWait = "1s"

// reachPairs is reaches as two lists of the same length, a field and a table
// it reaches at each place, for unnest.
func reachPairs(reaches map[uuid.UUID][]uuid.UUID) (fields, tables []uuid.UUID) {
	for f, ts := range reaches {
		for _, t := range ts {
			fields, tables = append(fields, f), append(tables, t)
		}
	}
	return fields, tables
}

// ComputedFillStart is what a fill of fields starts from: each one's
// generation, and the highest dirty mark among them. Marks up to it are made
// before any row the fill works out, so the fill answers them.
func ComputedFillStart(ctx context.Context, fields []uuid.UUID) (map[uuid.UUID]int64, int64, error) {
	gens, err := pgexec.Pairs[uuid.UUID, int64](ctx, `SELECT field_id, gen FROM data_table_computed WHERE field_id = ANY($1)`, fields)
	if err != nil {
		return nil, 0, err
	}
	mark, err := pgexec.Scalar[int64](ctx, `SELECT COALESCE(MAX(mark), 0) FROM data_table_computed_dirty WHERE field_id = ANY($1)`, fields)
	return gens, mark, err
}

// ComputedFill is a whole fill of one field: the generation it started from,
// the kind of value it gives ("" when the store can't keep it) and the
// tables its values reach.
type ComputedFill struct {
	Field   uuid.UUID
	Gen     int64
	Kind    string
	Reaches []uuid.UUID
}

// MarkComputedFilled records whole fills, in one statement: each field whose
// generation is still the one its fill started from has current entries from
// now, holding its kind and reaching exactly its reaches, and its dirty marks
// up to mark are answered. A field whose generation moved on meanwhile stays
// unfilled, for the next fill. It answers the fields it marked filled.
func MarkComputedFilled(ctx context.Context, fills []ComputedFill, mark int64) ([]uuid.UUID, error) {
	if len(fills) == 0 {
		return nil, nil
	}
	ids, gens, kinds := make([]uuid.UUID, len(fills)), make([]int64, len(fills)), make([]string, len(fills))
	reaches := make(map[uuid.UUID][]uuid.UUID, len(fills))
	for i, f := range fills {
		ids[i], gens[i], kinds[i] = f.Field, f.Gen, f.Kind
		reaches[f.Field] = f.Reaches
	}
	fields, tables := reachPairs(reaches)
	return pgexec.Query(ctx, `WITH f AS (
			SELECT * FROM unnest($1::uuid[], $2::bigint[], $3::text[]) AS f(id, gen, kind)
		), r AS (
			SELECT p.f, array_agg(DISTINCT p.t) AS reaches FROM unnest($4::uuid[], $5::uuid[]) AS p(f, t) GROUP BY p.f
		), filled AS (
			UPDATE data_table_computed c
			SET filled_gen = f.gen, filled_at = NOW(), kind = f.kind, reaches = COALESCE(r.reaches, '{}')
			FROM f LEFT JOIN r ON r.f = f.id
			WHERE c.field_id = f.id AND c.gen = f.gen
			RETURNING c.field_id
		), answered AS (
			DELETE FROM data_table_computed_dirty d USING filled WHERE d.field_id = filled.field_id AND d.mark <= $6
		)
		SELECT field_id FROM filled`,
		func(r pgexec.Row) (id uuid.UUID, err error) {
			err = r.Scan(&id)
			return id, err
		}, ids, gens, kinds, fields, tables, mark)
}

// UnfilledComputed is up to n stored fields whose entries aren't current:
// each one's table.
func UnfilledComputed(ctx context.Context, n int) (map[uuid.UUID]uuid.UUID, error) {
	return pgexec.Pairs[uuid.UUID, uuid.UUID](ctx, `SELECT c.field_id, c.table_id FROM data_table_computed c
		JOIN data_table_fields f ON f.id = c.field_id AND f.deleted_at IS NULL
		WHERE c.filled_gen <> c.gen LIMIT $1`, n)
}

// MarkComputedDirty marks, in a write's own transaction, the stored values
// a write to rows changes: the stored fields of each row's own table on the
// row, and, for the rows linking to or linked from them through up to three
// levels of links (rollups of rollups), the stored fields whose values reach
// one of tables, the tables written. A field the store found it can't keep,
// until a change raises its generation, has no values to mark. Nothing is
// marked when no such field is of or reaches those tables.
func MarkComputedDirty(ctx context.Context, tables, rows []uuid.UUID) error {
	if len(rows) == 0 {
		return nil
	}
	// The write holds its tables, and its rows', shared to its commit
	// (computedLock), before it looks for what reaches them: a fill reaching
	// one records that reach before this looks, or after this commits.
	if _, err := pgexec.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(`+computedLock+`)
		FROM (SELECT unnest($1::uuid[]) UNION SELECT table_id FROM data_table_rows WHERE id = ANY($2)) AS l(t)`, tables, rows); err != nil {
		return err
	}
	held, err := pgexec.Scalar[bool](ctx, `SELECT EXISTS (SELECT 1 FROM data_table_computed
		WHERE (table_id = ANY($1) OR reaches && $1::uuid[]) AND (kind <> '' OR filled_gen <> gen))`, tables)
	if err != nil || !held {
		return err
	}
	_, err = pgexec.Exec(ctx, `WITH RECURSIVE near(row_id, depth) AS (
			SELECT r, 0 FROM unnest($2::uuid[]) AS r
		UNION
			SELECT CASE WHEN l.to_row = n.row_id THEN l.from_row ELSE l.to_row END, n.depth + 1
			FROM near n JOIN data_table_links l ON l.to_row = n.row_id OR l.from_row = n.row_id
			WHERE n.depth < 3
		), nearest AS (
			SELECT row_id, MIN(depth) AS depth FROM near GROUP BY row_id
		)
		INSERT INTO data_table_computed_dirty (field_id, row_id)
		SELECT c.field_id, r.id
		FROM nearest n
		JOIN data_table_rows r ON r.id = n.row_id
		JOIN data_table_computed c ON c.table_id = r.table_id AND (c.kind <> '' OR c.filled_gen <> c.gen)
		WHERE n.depth = 0 OR c.reaches && $1::uuid[]`, tables, rows)
	return err
}

// DirtyComputed is up to n of fields' dirty marks, oldest first: their rows,
// each once, and the marks read, for AnswerDirty (n of them when more may
// wait); none when no row waits.
func DirtyComputed(ctx context.Context, fields []uuid.UUID, n int) ([]uuid.UUID, []int64, error) {
	type mark struct {
		row  uuid.UUID
		mark int64
	}
	marks, err := pgexec.Query(ctx, `SELECT row_id, mark FROM data_table_computed_dirty WHERE field_id = ANY($1) ORDER BY mark LIMIT $2`,
		func(r pgexec.Row) (m mark, err error) {
			err = r.Scan(&m.row, &m.mark)
			return m, err
		}, fields, n)
	if err != nil {
		return nil, nil, err
	}
	seen := make(map[uuid.UUID]bool, len(marks))
	var rows []uuid.UUID
	read := make([]int64, len(marks))
	for i, m := range marks {
		if !seen[m.row] {
			seen[m.row] = true
			rows = append(rows, m.row)
		}
		read[i] = m.mark
	}
	return rows, read, nil
}

// AnswerDirty deletes exactly the dirty marks read (DirtyComputed), by
// mark, the key of each (field, row, mark): their rows' entries were worked
// out again after each of them was made. Any other mark stays for the next
// pass, even one below a mark read, from a write that committed after the
// read: the rows were worked out from what the read could see, not that.
func AnswerDirty(ctx context.Context, marks []int64) error {
	if len(marks) == 0 {
		return nil
	}
	_, err := pgexec.Exec(ctx, `DELETE FROM data_table_computed_dirty WHERE mark = ANY($1)`, marks)
	return err
}

// DirtyTables is up to n tables with rows waiting to be worked out again.
func DirtyTables(ctx context.Context, n int) ([]uuid.UUID, error) {
	return pgexec.Query(ctx, `SELECT DISTINCT c.table_id FROM data_table_computed_dirty d
		JOIN data_table_computed c ON c.field_id = d.field_id LIMIT $1`,
		func(r pgexec.Row) (id uuid.UUID, err error) {
			err = r.Scan(&id)
			return id, err
		}, n)
}

// StoredFieldsOf is the stored fields of table, by id.
func StoredFieldsOf(ctx context.Context, table uuid.UUID) ([]uuid.UUID, error) {
	return pgexec.Query(ctx, `SELECT field_id FROM data_table_computed WHERE table_id = $1`,
		func(r pgexec.Row) (id uuid.UUID, err error) {
			err = r.Scan(&id)
			return id, err
		}, table)
}
