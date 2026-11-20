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
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
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
	return joinTx(ctx, postgresInit.DBConn.DBTimeout, func(dbctx context.Context, tx *sql.Tx) error {
		for _, f := range fields {
			if _, err := tx.ExecContext(dbctx, insertFieldSQL, f.Id, f.TableId, f.Name, f.Type, f.Config, f.Position,
				f.Description, f.Required, defaultOf(f)); err != nil {
				helpers.LogErrorWithContext(ctx, "models/CreateFieldsWithIDs err: %+v", err)
				return err
			}
		}
		return nil
	})
}

// DeleteFieldPair soft-deletes a relation field and the one showing its links
// from the other table, in one transaction; by is who deleted them. The
// other one may be gone already. Its links stay, unread through a deleted
// field, so restoring the field brings it back whole (RestoreFields); they go
// with it when the trash is purged (PurgeTrash).
func DeleteFieldPair(ctx context.Context, a, b *Field, by uuid.UUID) error {
	return joinTx(ctx, postgresInit.DBConn.DBTimeout, func(ctx context.Context, tx *sql.Tx) error {
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
	return joinTx(ctx, postgresInit.DBConn.DBTimeout, func(ctx context.Context, tx *sql.Tx) error {
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
	return joinTx(ctx, postgresInit.DBConn.DBTimeout, func(ctx context.Context, tx *sql.Tx) error {
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
// index alone, unless another vacuum of it is running. A VACUUM can't run in
// a transaction, so it runs alone (pgexec.ExecAlone), within vacuumTimeout.
func Vacuum(ctx context.Context, table string) error {
	if !insertMostly(table) {
		return fmt.Errorf("models: %s isn't vacuumed here", table)
	}
	_, err := pgexec.ExecAlone(ctx, vacuumTimeout, `VACUUM (ANALYZE, SKIP_LOCKED) `+table)
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

// TablesLinkedWith is the other tables whose relation fields link to these
// tables' rows, or whose rows their relation fields link to: those whose
// readers see their rows' names, or add them up.
func TablesLinkedWith(ctx context.Context, tableIds ...uuid.UUID) ([]uuid.UUID, error) {
	if len(tableIds) == 0 {
		return nil, nil
	}
	ids := make([]string, len(tableIds))
	asked := make(map[uuid.UUID]bool, len(tableIds))
	for i, id := range tableIds {
		ids[i], asked[id] = id.String(), true
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	// The ids twice: as uuids for the column (its index serves them), and as
	// text for the relation's config. One parameter can't be both types.
	rs, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, `SELECT DISTINCT CASE WHEN f.table_id = ANY($1::uuid[]) THEN f.config->>'table_id' ELSE f.table_id::text END
		FROM data_table_fields f JOIN data_tables t ON t.id = f.table_id AND t.deleted_at IS NULL
		WHERE f.deleted_at IS NULL AND f.type = 'relation' AND f.config->>'relation_target' = 'table'
			AND (f.table_id = ANY($1::uuid[]) OR f.config->>'table_id' = ANY($2::text[]))`, pq.Array(ids), pq.Array(ids))
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
		if id, err := uuid.Parse(s.String); err == nil && !asked[id] {
			out = append(out, id)
		}
	}
	return out, rs.Err()
}
