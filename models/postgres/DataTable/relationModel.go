package models

// Links between tables' rows (business/DataTable/relations.go), kept in
// data_table_links (migration 197): a relation field's link from a row of its
// table to a row of the table it links to, found from either end by index, in
// the order they were made. Links are made only between rows that exist
// (changeLinksMade's callers lock them first), and a row's links go when it's
// deleted (DeleteRows, which keeps them in its change set for a restore), so
// every link joins two rows that haven't been deleted. A deleted relation
// field's links stay until the trash is purged; nothing reads them through it.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ErrTooManyLinks is a link that would take a row past the links a cell can
// hold.
var ErrTooManyLinks = errors.New("too many links")

// Link is a relation field's link from a row of its table to a row of the
// table it links to.
type Link struct {
	From, To uuid.UUID
}

// FieldLinks is the links a write adds and removes through a relation field.
type FieldLinks struct {
	Field       uuid.UUID
	Add, Remove []Link
}

// linkEnds is the column of data_table_links a read starts from, and the
// one it finds: from_row to to_row, or the other way when to is set.
func linkEnds(to bool) (at, other string) {
	if to {
		return "to_row", "from_row"
	}
	return "from_row", "to_row"
}

// CountLinks counts a relation field's links from each of rows (to them,
// when to is set), at most upTo a row: a row with more counts upTo. Every
// row is in the answer.
func CountLinks(ctx context.Context, fieldId uuid.UUID, rows []uuid.UUID, upTo int, to bool) (map[uuid.UUID]int, error) {
	out := make(map[uuid.UUID]int, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	at, _ := linkEnds(to)
	q := `SELECT r.id, (SELECT COUNT(*) FROM (SELECT 1 FROM data_table_links l
			WHERE l.field_id = $1 AND l.` + at + ` = r.id LIMIT $3) x)
		FROM unnest($2::uuid[]) AS r(id)`
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rs, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, fieldId, pq.Array(rows), upTo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CountLinks err: %+v", err)
		return nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var id uuid.UUID
		var n int
		if err := rs.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rs.Err()
}

// LinksOf finds a relation field's links from each of rows (to them, when to
// is set): the first limits[i] row i has, in the order they were made. It
// returns each row's, by row.
func LinksOf(ctx context.Context, fieldId uuid.UUID, rows []uuid.UUID, limits []int, to bool) (map[uuid.UUID][]uuid.UUID, error) {
	out := make(map[uuid.UUID][]uuid.UUID, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	at, other := linkEnds(to)
	// Each row's first links, by index: its others aren't read.
	q := `SELECT r.id, x.other, x.created_at FROM unnest($2::uuid[], $3::int[]) AS r(id, k)
		CROSS JOIN LATERAL (SELECT l.` + other + ` AS other, l.created_at FROM data_table_links l
			WHERE l.field_id = $1 AND l.` + at + ` = r.id
			ORDER BY l.created_at, l.` + other + ` LIMIT r.k) x`
	k := make([]int64, len(limits))
	for i, n := range limits {
		k[i] = int64(n)
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rs, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, fieldId, pq.Array(rows), pq.Array(k))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/LinksOf err: %+v", err)
		return nil, err
	}
	defer rs.Close()
	type made struct {
		other uuid.UUID
		at    time.Time
	}
	found := map[uuid.UUID][]made{}
	for rs.Next() {
		var row uuid.UUID
		var m made
		if err := rs.Scan(&row, &m.other, &m.at); err != nil {
			return nil, err
		}
		found[row] = append(found[row], m)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	// Put in order here: sorting them all in the query takes longer than
	// reading them.
	for row, ms := range found {
		sort.Slice(ms, func(i, j int) bool {
			if !ms[i].at.Equal(ms[j].at) {
				return ms[i].at.Before(ms[j].at)
			}
			return bytes.Compare(ms[i].other[:], ms[j].other[:]) < 0
		})
		ids := make([]uuid.UUID, len(ms))
		for i, m := range ms {
			ids[i] = m.other
		}
		out[row] = ids
	}
	return out, nil
}

// RowsIn says which of ids are rows of a table: true for one that hasn't
// been deleted, false for one that has. Others aren't in the answer.
func RowsIn(ctx context.Context, tableId uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rs, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx,
		`SELECT id, deleted_at IS NULL FROM data_table_rows WHERE table_id = $1 AND id = ANY($2)`, tableId, pq.Array(ids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RowsIn err: %+v", err)
		return nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var id uuid.UUID
		var live bool
		if err := rs.Scan(&id, &live); err != nil {
			return nil, err
		}
		out[id] = live
	}
	return out, rs.Err()
}

// lockLinked locks the rows changes would link, and the rows also given, in
// one order, so two writes can't each wait for the other, and says which of
// them haven't been deleted. Held to the end of tx, the locks keep a row from
// being deleted as it's linked to, and two writes from each taking a row past
// the links it can have.
func lockLinked(ctx context.Context, tx *sql.Tx, changes []FieldLinks, also ...uuid.UUID) (map[uuid.UUID]bool, error) {
	seen := map[uuid.UUID]bool{}
	var ids []uuid.UUID
	add := func(id uuid.UUID) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, id := range also {
		add(id)
	}
	for _, c := range changes {
		for _, l := range c.Add {
			add(l.From)
			add(l.To)
		}
	}
	live := make(map[uuid.UUID]bool, len(ids))
	if len(ids) == 0 {
		return live, nil
	}
	rs, err := tx.QueryContext(ctx, `SELECT id FROM data_table_rows WHERE id = ANY($1) AND deleted_at IS NULL
		ORDER BY id FOR NO KEY UPDATE`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var id uuid.UUID
		if err := rs.Scan(&id); err != nil {
			return nil, err
		}
		live[id] = true
	}
	return live, rs.Err()
}

func linkPairs(ls []Link) (from, to []uuid.UUID) {
	for _, l := range ls {
		from, to = append(from, l.From), append(to, l.To)
	}
	return from, to
}

func emptyIfBlank(values string) string {
	if values == "" {
		return "{}"
	}
	return values
}

// CellsOf returns the rows of a table among ids that haven't been deleted,
// in no particular order, each with only its id, its cells keys, and what a
// field worked out from the row reads (its number, when it was made and last
// changed, and by whom).
func CellsOf(ctx context.Context, tableId uuid.UUID, ids []uuid.UUID, keys []string) ([]*Row, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	// The row's values read once: each -> reads all of them again.
	const q = `SELECT id, row_no, created_by, created_at, updated_at, updated_by,
		COALESCE((SELECT jsonb_object_agg(e.key, e.value) FROM jsonb_each(values) e WHERE e.key = ANY($3)), '{}'::jsonb)
		FROM data_table_rows WHERE table_id=$1 AND id = ANY($2) AND deleted_at IS NULL`
	// The ids go to pgx as they are: through pq.Array, pgx writes out the
	// whole list in an error before taking it, megabytes for a batch.
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, tableId, ids, pq.Array(keys))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CellsOf err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	out := make([]*Row, 0, len(ids))
	for rows.Next() {
		r := &Row{TableId: tableId}
		var rowNo sql.NullInt64
		var createdBy, updatedBy uuid.NullUUID
		if err := rows.Scan(&r.Id, &rowNo, &createdBy, &r.CreatedAt, &r.UpdatedAt, &updatedBy, &r.Values); err != nil {
			return nil, err
		}
		r.RowNo = rowNo.Int64
		if createdBy.Valid {
			r.CreatedBy = &createdBy.UUID
		}
		if updatedBy.Valid {
			r.UpdatedBy = &updatedBy.UUID
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRowIDs returns the ids of a table's rows in the table's order, a page
// at a time.
func ListRowIDs(ctx context.Context, tableId uuid.UUID, limit, offset int) ([]uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, `SELECT id FROM data_table_rows
		WHERE table_id=$1 AND deleted_at IS NULL `+rowOrder+` LIMIT $2 OFFSET $3`, tableId, limit, offset)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListRowIDs err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SearchRowsByCell finds a table's rows whose cell for a field holds text q,
// ignoring case: at most limit of them, in the table's order. A list's cell
// is matched as its JSON text, so its items count.
func SearchRowsByCell(ctx context.Context, tableId uuid.UUID, fieldId, q string, limit int) ([]*Row, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const sq = `SELECT ` + rowColumns + ` FROM data_table_rows
		WHERE table_id=$1 AND deleted_at IS NULL AND values->>$2 ILIKE $3 ESCAPE '\'
		` + rowOrder + ` LIMIT $4`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, sq, tableId, fieldId, "%"+escapeLikePattern(q)+"%", limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SearchRowsByCell err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*Row
	for rows.Next() {
		r, scanErr := scanRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateFieldsWithIDs inserts fields with the ids they're given, in one
// transaction: a relation field and the field showing its links from the
// other table, which point at each other.
func CreateFieldsWithIDs(ctx context.Context, fields ...*Field) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, f := range fields {
		if _, err := tx.ExecContext(dbctx, insertFieldSQL, f.Id, f.TableId, f.Name, f.Type, f.Config, f.Position,
			f.Description, f.Required, defaultOf(f)); err != nil {
			helpers.LogErrorWithContext(ctx, "models/CreateFieldsWithIDs err: %+v", err)
			return err
		}
	}
	return tx.Commit()
}

// DeleteFieldPair soft-deletes a relation field and the one showing its links
// from the other table, in one transaction; by is who deleted them. The
// other one may be gone already. Its links stay, unread through a deleted
// field, so restoring the field brings it back whole (RestoreFields); they go
// with it when the trash is purged (PurgeTrash).
func DeleteFieldPair(ctx context.Context, a, b *Field, by uuid.UUID) error {
	return postgresInit.InTx(ctx, postgresInit.DBConn.DBTimeout, func(ctx context.Context, tx *sql.Tx) error {
		if err := deleteField(ctx, tx, a.TableId, a.Id, by); err != nil {
			return err
		}
		if b != nil {
			if err := deleteField(ctx, tx, b.TableId, b.Id, by); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return nil
	})
}

// ErrRelationGone is the field showing a relation's links brought back while
// the relation is deleted.
var ErrRelationGone = errors.New("the relation whose links it shows is deleted")

// DeleteShowingField soft-deletes f, a field showing the links of a relation
// field in table owners, in one transaction with every live field there that
// says f shows its links ("inverse") no longer saying so. Their links stay,
// shown in their own table alone. by is who deleted f.
func DeleteShowingField(ctx context.Context, f *Field, owners, by uuid.UUID) error {
	return postgresInit.InTx(ctx, postgresInit.DBConn.DBTimeout, func(ctx context.Context, tx *sql.Tx) error {
		if err := deleteField(ctx, tx, f.TableId, f.Id, by); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE data_table_fields SET config = config - 'inverse', updated_at = NOW()
			WHERE table_id = $1 AND deleted_at IS NULL AND config ->> 'inverse' = $2`, owners, f.Id.String()); err != nil {
			helpers.LogErrorWithContext(ctx, "models/DeleteShowingField relation err: %+v", err)
			return err
		}
		return nil
	})
}

// RestoreShowingField brings back from the trash f, a field showing the links
// of relation field owner of table owners, in one transaction with owner
// showing its links there again when it shows them in no other field. It is
// ErrRelationGone when owner is deleted, as it can't show links then.
func RestoreShowingField(ctx context.Context, f *Field, owner, owners uuid.UUID) error {
	return postgresInit.InTx(ctx, postgresInit.DBConn.DBTimeout, func(ctx context.Context, tx *sql.Tx) error {
		var shown bool
		err := tx.QueryRowContext(ctx, `SELECT config ? 'inverse' FROM data_table_fields
			WHERE id = $1 AND table_id = $2 AND deleted_at IS NULL FOR UPDATE`, owner, owners).Scan(&shown)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRelationGone
		}
		if err != nil {
			helpers.LogErrorWithContext(ctx, "models/RestoreShowingField relation err: %+v", err)
			return err
		}
		if !shown {
			if _, err := tx.ExecContext(ctx, `UPDATE data_table_fields SET config = config || jsonb_build_object('inverse', $2::text), updated_at = NOW()
				WHERE id = $1`, owner, f.Id.String()); err != nil {
				helpers.LogErrorWithContext(ctx, "models/RestoreShowingField relation err: %+v", err)
				return err
			}
		}
		return restoreField(ctx, tx, f)
	})
}

// vacuumTimeout is how long a vacuum of one of the insert-mostly tables may
// take.
const vacuumTimeout = 10 * time.Minute

// InsertMostly is the tables' tables that are mostly added to, which Postgres
// 12 never vacuums on its own (business/DataTable linkUpkeep.go vacuums them):
// the links between rows, and the row ledger's change sets and changes.
var InsertMostly = []string{"data_table_links", "data_table_change_sets", "data_table_changes"}

// insertMostly is whether table is one of InsertMostly: the only names a
// vacuum here takes, as a name can't be a statement's parameter.
func insertMostly(table string) bool {
	for _, t := range InsertMostly {
		if t == table {
			return true
		}
	}
	return false
}

// TuplesChanged is how many rows of one of InsertMostly have been added and
// removed in all, as Postgres has counted them: it only ever grows, until
// its counts are reset.
func TuplesChanged(ctx context.Context, table string) (int64, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var n int64
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, `SELECT COALESCE(SUM(n_tup_ins + n_tup_del), 0)
		FROM pg_stat_user_tables WHERE relname = $1`, table).Scan(&n)
	return n, err
}

// Vacuum vacuums one of InsertMostly, so counts and reads of it take the
// index alone, unless another vacuum of it is running.
func Vacuum(ctx context.Context, table string) error {
	if !insertMostly(table) {
		return fmt.Errorf("models: %s isn't vacuumed here", table)
	}
	dbctx, cancel := context.WithTimeout(ctx, vacuumTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, `VACUUM (ANALYZE, SKIP_LOCKED) `+table)
	return err
}

// RowHasLinks is whether a row links to rows, or rows link to it.
func RowHasLinks(ctx context.Context, rowId uuid.UUID) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var has bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, `SELECT EXISTS (SELECT 1 FROM data_table_links WHERE from_row = $1)
		OR EXISTS (SELECT 1 FROM data_table_links WHERE to_row = $1)`, rowId).Scan(&has)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RowHasLinks err: %+v", err)
	}
	return has, err
}

// TablesLinkedWith is the other tables whose relation fields link to a
// table's rows, or whose rows its relation fields link to: those whose
// readers see its rows' names, or add them up.
func TablesLinkedWith(ctx context.Context, tableId uuid.UUID) ([]uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rs, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, `SELECT DISTINCT CASE WHEN f.table_id = $1 THEN f.config->>'table_id' ELSE f.table_id::text END
		FROM data_table_fields f JOIN data_tables t ON t.id = f.table_id AND t.deleted_at IS NULL
		WHERE f.deleted_at IS NULL AND f.type = 'relation' AND f.config->>'relation_target' = 'table'
			AND (f.table_id = $1 OR f.config->>'table_id' = $1::text)`, tableId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TablesLinkedWith err: %+v", err)
		return nil, err
	}
	defer rs.Close()
	var out []uuid.UUID
	for rs.Next() {
		var s sql.NullString
		if err := rs.Scan(&s); err != nil {
			return nil, err
		}
		if id, err := uuid.Parse(s.String); err == nil && id != tableId {
			out = append(out, id)
		}
	}
	return out, rs.Err()
}
