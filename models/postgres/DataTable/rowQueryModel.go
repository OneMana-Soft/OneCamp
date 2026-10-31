package models

// Running a compiled row query (business/RowQuery, tables A3): the business
// layer writes the condition and the order from a spec, with every value a
// parameter from $2 ($1 is the table); here they run against a table's live
// rows, aliased r. Every statement runs through models/pgexec.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// KeyedRow is a row a query found, and its values for the query's order
// (numbers as text, moments as time.Time, nil when empty), for the next
// window's cursor.
type KeyedRow struct {
	ID   uuid.UUID
	Keys []interface{}
}

// QueryRowWindow is up to limit of table's live rows that meet where, in
// order, with each row's keys (none asked, none read: a list of ids).
func QueryRowWindow(ctx context.Context, table uuid.UUID, joins, where string, keys []string, order string, args []interface{}, limit int) ([]KeyedRow, error) {
	return keyedRows(ctx, table, joins, where, keys, order, args, limit)
}

// walkSettings make a walk follow its index (business/RowQuery walks.go).
// The planner can't know how many of one field's entries hold a value (it
// sees every field's together), so it guessed a few hundred where 25,000
// matched, and sorted them all where the index's own order stops at the
// window's end. Within the walk's own transaction it reads the index in
// order, probing each row's other cells (the walk keeps its filter's
// EXISTS from being joined), and stops at limit.
var walkSettings = []string{
	"SET LOCAL enable_sort = off",
	"SET LOCAL enable_bitmapscan = off",
	"SET LOCAL enable_hashjoin = off",
	"SET LOCAL max_parallel_workers_per_gather = 0",
	// Too little memory to hash a filter's every entry: each walked row's
	// cells are probed by the primary key, so a window costs its own rows.
	"SET LOCAL work_mem = '64kB'",
}

// ErrWalkTooLong is a walk that ran past its budget (QueryRowWalk).
var ErrWalkTooLong = errors.New("the walk ran past its budget")

// ErrSummariesTooLong is a summaries read that ran past its budget
// (SummarizeRowsWhere).
var ErrSummariesTooLong = errors.New("the summaries ran past their budget")

// inBudget runs fn in a transaction of its own with settings in place and,
// for a budget above zero, a statement_timeout at it. The transaction has a
// statement's DBTimeout (pgexec.InTx's TxTimeout is longer). stopped says the
// budget stopped fn.
func inBudget(ctx context.Context, settings []string, budget time.Duration, fn func(ctx context.Context) error) (stopped bool, err error) {
	err = postgresInit.InTx(ctx, postgresInit.DBConn.DBTimeout, func(ctx context.Context, tx *sql.Tx) error {
		ctx = pgexec.WithTx(ctx, tx)
		if budget > 0 {
			settings = append(append([]string{}, settings...), fmt.Sprintf("SET LOCAL statement_timeout = %d", max(budget.Milliseconds(), 1)))
		}
		for _, s := range settings {
			if _, err := pgexec.Exec(ctx, s); err != nil {
				return err
			}
		}
		return fn(ctx)
	})
	return budget > 0 && helpers.IsQueryCanceled(err), err
}

// QueryRowWalk is QueryRowWindow along the order's own index: a walk's query,
// in a transaction of its own that has the planner read that index in order
// (inBudget). A budget above zero stops the statement at it, as
// ErrWalkTooLong.
func QueryRowWalk(ctx context.Context, table uuid.UUID, joins, where string, keys []string, order string, args []interface{}, limit int, budget time.Duration) ([]KeyedRow, error) {
	var out []KeyedRow
	stopped, err := inBudget(ctx, walkSettings, budget, func(ctx context.Context) (err error) {
		out, err = keyedRows(ctx, table, joins, where, keys, order, args, limit)
		return err
	})
	if stopped {
		return nil, ErrWalkTooLong
	}
	return out, err
}

// keyedRows runs a window's query, in the transaction ctx carries if any.
func keyedRows(ctx context.Context, table uuid.UUID, joins, where string, keys []string, order string, args []interface{}, limit int) ([]KeyedRow, error) {
	sel := []string{"r.id"}
	for _, k := range keys {
		sel = append(sel, "("+k+")")
	}
	q := fmt.Sprintf(`SELECT %s FROM data_table_rows r %s
		WHERE r.table_id = $1 AND r.deleted_at IS NULL AND (%s) %s LIMIT %d`, strings.Join(sel, ", "), joins, where, order, limit)
	out, err := pgexec.Query(ctx, q, func(r pgexec.Row) (KeyedRow, error) {
		kr := KeyedRow{Keys: make([]interface{}, len(keys))}
		dest := make([]interface{}, len(keys)+1)
		dest[0] = &kr.ID
		for i := range kr.Keys {
			dest[i+1] = &kr.Keys[i]
		}
		if err := r.Scan(dest...); err != nil {
			return kr, err
		}
		for i, k := range kr.Keys {
			switch v := k.(type) {
			case []byte:
				kr.Keys[i] = string(v)
			case time.Time:
				kr.Keys[i] = v.UTC()
			}
		}
		return kr, nil
	}, append([]interface{}{table}, args...)...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/QueryRowWindow err: %+v", err)
	}
	return out, err
}

// CountRowsWhere is how many of table's live rows meet where, counting at
// most limit+1, so a count past limit says it is.
func CountRowsWhere(ctx context.Context, table uuid.UUID, where string, args []interface{}, limit int) (int, error) {
	q := fmt.Sprintf(`SELECT COUNT(*) FROM (SELECT 1 FROM data_table_rows r
		WHERE r.table_id = $1 AND r.deleted_at IS NULL AND (%s) LIMIT %d) n`, where, limit+1)
	n, err := pgexec.Scalar[int](ctx, q, append([]interface{}{table}, args...)...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CountRowsWhere err: %+v", err)
		return 0, err
	}
	return n, nil
}

// SummarizeRowsWhere reads aggregates, selects, over table's live rows that
// meet where, each with the entries joins brings to it: one row over them
// all, or, given grouped (a GROUP BY with its ORDER BY and LIMIT), a row a
// group. scan reads each row. It runs in a transaction of its own
// (inBudget): a budget above zero stops the read at it, as
// ErrSummariesTooLong.
func SummarizeRowsWhere(ctx context.Context, table uuid.UUID, joins, selects, where, grouped string, args []interface{}, budget time.Duration, scan func(pgexec.Row) error) error {
	q := fmt.Sprintf(`SELECT %s FROM data_table_rows r %s
		WHERE r.table_id = $1 AND r.deleted_at IS NULL AND (%s) %s`, selects, joins, where, grouped)
	stopped, err := inBudget(ctx, nil, budget, func(ctx context.Context) error {
		_, err := pgexec.Query(ctx, q, func(r pgexec.Row) (struct{}, error) {
			return struct{}{}, scan(r)
		}, append([]interface{}{table}, args...)...)
		return err
	})
	if stopped {
		return ErrSummariesTooLong
	}
	return err
}

// RowsByIDs is table's live rows among ids, whole, in no particular order.
func RowsByIDs(ctx context.Context, table uuid.UUID, ids []uuid.UUID) ([]*Row, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	q := `SELECT ` + rowColumns + ` FROM data_table_rows WHERE table_id = $1 AND id = ANY($2) AND deleted_at IS NULL`
	out, err := pgexec.Query(ctx, q, func(r pgexec.Row) (*Row, error) { return scanRow(r) }, table, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RowsByIDs err: %+v", err)
	}
	return out, err
}

// HasUnkeyedRows is whether table has live rows with no place key yet (rows a
// server from before the keys made; StartLinkUpkeep keys them). One lookup
// of the unkeyed index, which is empty almost always.
func HasUnkeyedRows(ctx context.Context, table uuid.UUID) (bool, error) {
	has, err := pgexec.Scalar[bool](ctx, `SELECT EXISTS (SELECT 1 FROM data_table_rows
		WHERE table_id = $1 AND position_key IS NULL AND deleted_at IS NULL)`, table)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/HasUnkeyedRows err: %+v", err)
	}
	return has, err
}

// FieldHasPlaceAbove is whether a choice field's entries hold an option
// place above n: a label no option has (business/RowQuery walks.go). One
// lookup of the field's (n) index.
func FieldHasPlaceAbove(ctx context.Context, field uuid.UUID, n float64) (bool, error) {
	// The field's greatest place, read from the end of its index on n. EXISTS
	// of a place above n was planned as a scan of the whole index for the
	// first one, and read every entry when there was none (120 to 190 ms a
	// window at 100,000 rows).
	top, found, err := pgexec.QueryOne(ctx, `SELECT n FROM data_table_cell_index
		WHERE field_id = $1 AND n IS NOT NULL ORDER BY n DESC LIMIT 1`, func(r pgexec.Row) (top sql.NullFloat64, err error) {
		err = r.Scan(&top)
		return top, err
	}, field)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/FieldHasPlaceAbove err: %+v", err)
		return false, err
	}
	return found && top.Valid && top.Float64 > n, nil
}

// FieldHolds is whether a field's entries hold a value: as text t (a label),
// any tick ("b"), or none ("empty", a walked field's empty cell). Entries of
// rows in the trash count, as a cheap answer that may say yes for nothing
// (business/RowQuery passes a group over only on a no), as a t longer than
// the t index holds is answered. One lookup of an index the field leads.
func FieldHolds(ctx context.Context, field uuid.UUID, kind, t string) (bool, error) {
	// The first entry in the order of the index the condition names: as
	// EXISTS, the planner could take a scan of the whole cell index to be
	// cheaper, betting on a match early (the work budget test caught it on a
	// checkbox); in an index's own order a scan of the table needs a sort of
	// it all, so the index is read.
	q := `SELECT 1 FROM data_table_cell_index WHERE field_id = $1 AND `
	args := []interface{}{field}
	switch kind {
	case "t":
		if len(t) > IndexedTextBytes {
			return true, nil
		}
		q += `t = $2::text COLLATE "C" AND ` + ShortText("t") + ` ORDER BY p, row_id`
		args = append(args, t)
	case "b":
		q += `b IS NOT NULL ORDER BY b, p, row_id`
	default:
		q += `n IS NULL AND t IS NULL AND d IS NULL AND b IS NULL AND a IS NULL ORDER BY p, row_id`
	}
	_, found, err := pgexec.QueryOne(ctx, q+` LIMIT 1`, func(r pgexec.Row) (one int, err error) {
		err = r.Scan(&one)
		return one, err
	}, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/FieldHolds err: %+v", err)
		return false, err
	}
	return found, nil
}
