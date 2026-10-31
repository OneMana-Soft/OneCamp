package models

// The cell index (migration 302, tables A3): each stored cell's value typed
// for comparing and sorting, which business/RowQuery works out and keeps in
// the write's own transaction, and a table's mark once all of it is there.
//
// Every statement here runs through models/pgexec. Those a write must make
// in its own transaction (ReplaceCellIndex, SyncPlaces, LiveFields,
// RowsForIndex) run in the one ctx carries: a row ledger observer puts the
// write's there with pgexec.WithTx, and the repair each batch's.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// IndexCell is one cell's entry: at most one of N, T, D and A, or B true; a
// choice has N and T both.
type IndexCell struct {
	Table, Field, Row uuid.UUID
	N                 *float64
	T                 *string
	D                 *time.Time
	B                 bool
	A                 []string
	// S is a text's, or a choice's label's, natural sort key
	// (business/RowQuery naturalKey): what a sort reads, where T is what a
	// filter reads. Set whenever T is, for those.
	S *string
	// P is the row's place when the writer has the row (a whole row's
	// entries); nil, and it is read from the row (SyncPlaces).
	P *string
}

// indexBatch is how many entries one INSERT writes.
const indexBatch = 500

// IndexedTextBytes is the longest t the t index (idx_cell_index_t) holds:
// Postgres refuses a btree entry over 2,704 bytes, and an entry holding a
// long text's whole t failed the write of it (T5c). Its predicate and a
// query reading it say the same words (ShortText), so the planner can use
// it; a filter reads the whole t still, from each row's own entry.
const IndexedTextBytes = 2000

// ShortText is the condition that column (a t) is one the t index holds, in
// the words of its predicate.
func ShortText(column string) string {
	return "octet_length(" + column + ") <= " + strconv.Itoa(IndexedTextBytes)
}

// ReplaceCellIndex puts cells in place of the index's entries for rows'
// fields (every field's when fields is empty), in the transaction ctx
// carries. Fields named, it is ReplaceCells for each of rows' cells of them.
func ReplaceCellIndex(ctx context.Context, rows, fields []uuid.UUID, cells []IndexCell) error {
	if len(rows) == 0 {
		return nil
	}
	if len(fields) > 0 {
		cellRows := make([]uuid.UUID, 0, len(rows)*len(fields))
		cellFields := make([]uuid.UUID, 0, len(rows)*len(fields))
		for _, r := range rows {
			for _, f := range fields {
				cellRows, cellFields = append(cellRows, r), append(cellFields, f)
			}
		}
		return ReplaceCells(ctx, cellRows, cellFields, cells)
	}
	if _, err := pgexec.Exec(ctx, `DELETE FROM data_table_cell_index WHERE row_id = ANY($1)`, rows); err != nil {
		helpers.LogErrorWithContext(ctx, "models/ReplaceCellIndex delete err: %+v", err)
		return err
	}
	return putEntries(ctx, cells)
}

// ReplaceCells puts cells in place of the index's entries of rows[i]'s
// fields[i], in the transaction ctx carries, and their rows' other entries
// stay. Each entry is written over its cell's own (putEntries), and then the
// cells left with none lose theirs, in one statement. Deleting every cell's
// entry first read a field's entries through on its first fill (V-114): the
// planner, which hadn't counted a new field's entries, took them for a
// handful along the field's own index and kept the batch's rows, so each
// batch read all the batches before it had written, to delete nothing.
func ReplaceCells(ctx context.Context, rows, fields []uuid.UUID, cells []IndexCell) error {
	if len(rows) == 0 {
		return nil
	}
	if err := putEntries(ctx, cells); err != nil {
		return err
	}
	written := make(map[[2]uuid.UUID]bool, len(cells))
	for _, c := range cells {
		written[[2]uuid.UUID{c.Row, c.Field}] = true
	}
	var emptyRows, emptyFields []uuid.UUID
	for i, r := range rows {
		if !written[[2]uuid.UUID{r, fields[i]}] {
			emptyRows, emptyFields = append(emptyRows, r), append(emptyFields, fields[i])
		}
	}
	if len(emptyRows) == 0 {
		return nil
	}
	if _, err := pgexec.Exec(ctx, `DELETE FROM data_table_cell_index ci
		USING unnest($1::uuid[], $2::uuid[]) AS c(row_id, field_id)
		WHERE ci.row_id = c.row_id AND ci.field_id = c.field_id`, emptyRows, emptyFields); err != nil {
		helpers.LogErrorWithContext(ctx, "models/ReplaceCells delete err: %+v", err)
		return err
	}
	return nil
}

// putEntries writes cells' entries, indexBatch to an INSERT, and then the
// place of each row whose writer didn't have it (SyncPlaces).
func putEntries(ctx context.Context, cells []IndexCell) error {
	var unplaced []uuid.UUID
	for _, c := range cells {
		if c.P == nil {
			unplaced = append(unplaced, c.Row)
		}
	}
	for len(cells) > 0 {
		batch := cells[:min(indexBatch, len(cells))]
		cells = cells[len(batch):]
		var sb strings.Builder
		sb.WriteString(`INSERT INTO data_table_cell_index (table_id, field_id, row_id, n, t, d, b, a, p, s) VALUES `)
		args := make([]interface{}, 0, 10*len(batch))
		for i, c := range batch {
			if i > 0 {
				sb.WriteString(", ")
			}
			n := len(args)
			fmt.Fprintf(&sb, "($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)", n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9, n+10)
			var b interface{}
			if c.B {
				b = true
			}
			var a interface{}
			if c.A != nil {
				a = pq.Array(c.A)
			}
			args = append(args, c.Table, c.Field, c.Row, c.N, c.T, c.D, b, a, c.P, c.S)
		}
		sb.WriteString(` ON CONFLICT (row_id, field_id) DO UPDATE SET n = EXCLUDED.n, t = EXCLUDED.t, d = EXCLUDED.d, b = EXCLUDED.b, a = EXCLUDED.a, p = EXCLUDED.p, s = EXCLUDED.s`)
		if _, err := pgexec.Exec(ctx, sb.String(), args...); err != nil {
			helpers.LogErrorWithContext(ctx, "models/putEntries insert err: %+v", err)
			return err
		}
	}
	return SyncPlaces(ctx, unplaced)
}

// SyncPlaces copies rows' places (position_key) to their entries' p, in the
// transaction ctx carries: a sort on a field reads the index in (value,
// place, id) order, so a tie keeps the table's order without reading the
// rows. Every write that sets a place calls it: an entry's own write, a move
// (the ledger's observer), and keying rows that had none (keyRowsWithoutTx).
func SyncPlaces(ctx context.Context, rows []uuid.UUID) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := pgexec.Exec(ctx, `UPDATE data_table_cell_index ci SET p = r.position_key
		FROM data_table_rows r WHERE r.id = ci.row_id AND ci.row_id = ANY($1) AND ci.p IS DISTINCT FROM r.position_key`, rows)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SyncPlaces err: %+v", err)
	}
	return err
}

// PlaceChoices sets a choice field's entries' option places (n) by their
// labels (t): a label's place is where it first stands in labels, from 1,
// and a label not there sorts after them all, at unknown. Only the entries
// whose place changes are written; the field's entries are read once, each
// label's place found by a hash of the few labels. In the transaction ctx
// carries.
func PlaceChoices(ctx context.Context, field uuid.UUID, labels []string, unknown float64) error {
	_, err := pgexec.Exec(ctx, `UPDATE data_table_cell_index ci SET n = placed.n
		FROM (SELECT x.row_id, COALESCE(o.place, $3::numeric) AS n
		      FROM data_table_cell_index x
		      LEFT JOIN (SELECT label COLLATE "C" AS label, min(i)::numeric AS place
		                 FROM unnest($2::text[]) WITH ORDINALITY AS u(label, i) GROUP BY 1) o ON o.label = x.t
		      WHERE x.field_id = $1 AND x.t IS NOT NULL) placed
		WHERE ci.field_id = $1 AND ci.row_id = placed.row_id AND ci.n IS DISTINCT FROM placed.n`, field, pq.Array(labels), unknown)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PlaceChoices err: %+v", err)
	}
	return err
}

// LiveFields is a table's live fields as the transaction ctx carries sees
// them (a type changed in it); ListFields reads them on the pool.
func LiveFields(ctx context.Context, table uuid.UUID) ([]*Field, error) {
	return pgexec.Query(ctx, `SELECT `+fieldColumns+` FROM data_table_fields WHERE table_id = $1 AND deleted_at IS NULL ORDER BY position, created_at`,
		func(r pgexec.Row) (*Field, error) { return scanField(r) }, table)
}

// RowsForIndex is a table's live rows among ids, read FOR SHARE in the
// transaction ctx carries: a write in progress finishes first, and none
// starts until that transaction ends, so what is indexed is what is stored.
func RowsForIndex(ctx context.Context, table uuid.UUID, ids []uuid.UUID) ([]*Row, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `SELECT `+rowColumns+` FROM data_table_rows WHERE table_id = $1 AND id = ANY($2) AND deleted_at IS NULL FOR SHARE`,
		func(r pgexec.Row) (*Row, error) { return scanRow(r) }, table, ids)
}

// scanID reads a row of one id.
func scanID(r pgexec.Row) (id uuid.UUID, err error) {
	err = r.Scan(&id)
	return id, err
}

// RowIDsAfter is up to n of a table's live rows' ids after after, in id
// order: a table indexed a batch at a time, in the transaction ctx carries
// when it carries one (a field's every row, reindexed in a write's).
func RowIDsAfter(ctx context.Context, table, after uuid.UUID, n int) ([]uuid.UUID, error) {
	return pgexec.Query(ctx, `SELECT id FROM data_table_rows WHERE table_id = $1 AND id > $2 AND deleted_at IS NULL ORDER BY id LIMIT $3`, scanID, table, after, n)
}

// scanInt reads a row of one integer.
func scanInt(r pgexec.Row) (n int, err error) {
	err = r.Scan(&n)
	return n, err
}

// TablesWithoutIndex is up to n live tables whose cell index isn't whole, or
// was made whole by an older version than version (what its entries hold,
// business/RowQuery IndexVersion), oldest first. A table a pass couldn't
// index isn't among them until its back-off ends (MarkCellIndexFailed).
func TablesWithoutIndex(ctx context.Context, n, version int) ([]uuid.UUID, error) {
	return pgexec.Query(ctx, `SELECT id FROM data_tables WHERE (cell_index_at IS NULL OR cell_index_version < $2) AND deleted_at IS NULL
		AND (cell_index_retry_at IS NULL OR cell_index_retry_at <= NOW())
		ORDER BY created_at LIMIT $1`, scanID, n, version)
}

// CellIndexStaleGen is how many times a table's mark has been taken away
// (MarkCellIndexStale), 0 for no such table: a walk that makes its cell index
// whole reads it first, and marks the table only while it holds
// (MarkCellIndexed).
func CellIndexStaleGen(ctx context.Context, table uuid.UUID) (int, error) {
	n, _, err := pgexec.QueryOne(ctx, `SELECT cell_index_stale_gen FROM data_tables WHERE id = $1`, scanInt, table)
	return n, err
}

// MarkCellIndexed says a table's cell index is whole as of now, made by
// version, and clears the failures the repair sweep recorded on it, unless
// its mark was taken away since the walk that made it whole began: gen is
// CellIndexStaleGen as that walk read it before its first row (V-36). It
// says whether it marked the table, and how many passes in a row had failed
// it before this.
func MarkCellIndexed(ctx context.Context, table uuid.UUID, version, gen int) (marked bool, failed int, err error) {
	failed, marked, err = pgexec.QueryOne(ctx, `WITH was AS (SELECT cell_index_failures FROM data_tables WHERE id = $1)
		UPDATE data_tables SET cell_index_at = NOW(), cell_index_version = $2, cell_index_failures = 0, cell_index_retry_at = NULL
		WHERE id = $1 AND cell_index_stale_gen = $3 RETURNING (SELECT cell_index_failures FROM was)`, scanInt, table, version, gen)
	return marked, failed, err
}

// MarkCellIndexFailed records that a pass of the repair sweep couldn't make a
// table's cell index whole: one more failure in a row, and no pass takes it
// again until wait(that many) from now (business/RowQuery's back-off). It
// says how many passes in a row have failed it, this one counted.
func MarkCellIndexFailed(ctx context.Context, table uuid.UUID, wait func(failures int) time.Duration) (int, error) {
	n, _, err := pgexec.QueryOne(ctx, `UPDATE data_tables SET cell_index_failures = cell_index_failures + 1 WHERE id = $1 RETURNING cell_index_failures`, scanInt, table)
	if err != nil {
		return 0, err
	}
	_, err = pgexec.Exec(ctx, `UPDATE data_tables SET cell_index_retry_at = NOW() + make_interval(secs => $2::float8) WHERE id = $1`, table, wait(n).Seconds())
	return n, err
}

// MarkCellIndexStale takes away a table's mark, so the repair sweep makes
// its cell index whole again (a field's entries that couldn't be written
// when it was made), and Row Query answers by the evaluator until then. It
// counts the time (cell_index_stale_gen), so a walk already under way
// doesn't put the mark back (MarkCellIndexed).
func MarkCellIndexStale(ctx context.Context, table uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE data_tables SET cell_index_at = NULL, cell_index_stale_gen = cell_index_stale_gen + 1 WHERE id = $1`, table)
	return err
}

// CellIndexState is whether a table's cell index is whole, and the version
// that made it so (business/RowQuery IndexVersion: what its entries hold).
func CellIndexState(ctx context.Context, table uuid.UUID) (whole bool, version int, err error) {
	type state struct {
		at      sql.NullTime
		version int
	}
	s, found, err := pgexec.QueryOne(ctx, `SELECT cell_index_at, cell_index_version FROM data_tables WHERE id = $1`, func(r pgexec.Row) (s state, err error) {
		err = r.Scan(&s.at, &s.version)
		return s, err
	}, table)
	if err != nil || !found {
		return false, 0, err
	}
	return s.at.Valid, s.version, nil
}

// AnalyzeRows has the planner count the rows and the cell index again
// (business/RowQuery IndexTable, after a table filled at once). Sampling two
// large tables can take longer than a statement's DBTimeout, or pgexec.InTx's
// TxTimeout, so it keeps BulkTimeout: in a transaction of its own with that
// bound, which pgexec runs it in.
func AnalyzeRows(ctx context.Context) error {
	err := postgresInit.InTx(ctx, postgresInit.BulkTimeout, func(ctx context.Context, tx *sql.Tx) error {
		_, err := pgexec.Exec(pgexec.WithTx(ctx, tx), `ANALYZE data_table_rows, data_table_cell_index`)
		return err
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AnalyzeRows err: %+v", err)
	}
	return err
}
