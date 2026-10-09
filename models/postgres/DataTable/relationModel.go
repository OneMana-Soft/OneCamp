package models

// Links between tables' rows (business/DataTable/relations.go), kept in
// data_table_links (migration 197): a relation field's link from a row of its
// table to a row of the table it links to, found from either end by index, in
// the order they were made. Links are made only between rows that exist
// (changeLinks locks them first), and a row's links go when it's deleted
// (DeleteRow), so every link joins two rows that haven't been deleted.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
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

// changeLinks makes link changes in tx, adding only links between rows live
// says exist, each after those made before, in the order given, and says how
// many it made and removed. A row that would link to more than maxPerRow
// rows through a field makes it ErrTooManyLinks.
func changeLinks(ctx context.Context, tx *sql.Tx, changes []FieldLinks, live map[uuid.UUID]bool, maxPerRow int) (added, removed int, err error) {
	for _, c := range changes {
		if len(c.Remove) > 0 {
			from, to := linkPairs(c.Remove)
			res, err := tx.ExecContext(ctx, `DELETE FROM data_table_links l USING unnest($2::uuid[], $3::uuid[]) AS p(f, t)
				WHERE l.field_id = $1 AND l.from_row = p.f AND l.to_row = p.t`, c.Field, pq.Array(from), pq.Array(to))
			if err != nil {
				return 0, 0, err
			}
			n, _ := res.RowsAffected()
			removed += int(n)
		}
		var add []Link
		for _, l := range c.Add {
			if live[l.From] && live[l.To] {
				add = append(add, l)
			}
		}
		if len(add) == 0 {
			continue
		}
		from, to := linkPairs(add)
		// Made a microsecond apart, so they keep the order given.
		res, err := tx.ExecContext(ctx, `INSERT INTO data_table_links (field_id, from_row, to_row, created_at)
			SELECT $1, p.f, p.t, NOW() + p.i * INTERVAL '1 microsecond' FROM unnest($2::uuid[], $3::uuid[]) WITH ORDINALITY AS p(f, t, i)
			ON CONFLICT DO NOTHING`, c.Field, pq.Array(from), pq.Array(to))
		if err != nil {
			return 0, 0, err
		}
		n, _ := res.RowsAffected()
		added += int(n)
		var over int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT from_row FROM data_table_links
			WHERE field_id = $1 AND from_row = ANY($2) GROUP BY from_row HAVING COUNT(*) > $3) x`,
			c.Field, pq.Array(from), maxPerRow).Scan(&over); err != nil {
			return 0, 0, err
		}
		if over > 0 {
			return 0, 0, ErrTooManyLinks
		}
	}
	return added, removed, nil
}

// ChangeLinks adds and removes a relation field's links in one transaction,
// and says how many it made and removed: links there already, and links to or
// from a row deleted meanwhile, aren't made. A row that would link to more
// than maxPerRow rows through the field makes it ErrTooManyLinks, and nothing
// changes.
func ChangeLinks(ctx context.Context, change FieldLinks, maxPerRow int) (added, removed int, err error) {
	if len(change.Add) == 0 && len(change.Remove) == 0 {
		return 0, 0, nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	changes := []FieldLinks{change}
	live, err := lockLinked(dbctx, tx, changes)
	if err == nil {
		added, removed, err = changeLinks(dbctx, tx, changes, live, maxPerRow)
	}
	if err != nil {
		if !errors.Is(err, ErrTooManyLinks) {
			helpers.LogErrorWithContext(ctx, "models/ChangeLinks err: %+v", err)
		}
		return 0, 0, err
	}
	return added, removed, tx.Commit()
}

// CreateRowWithLinks inserts a row (with r.Id, when it's set) and adds links
// to and from it, in one transaction.
func CreateRowWithLinks(ctx context.Context, r *Row, changes []FieldLinks, maxPerRow int) (*Row, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	id := r.Id
	if id == uuid.Nil {
		id = uuid.New()
	}
	const q = `INSERT INTO data_table_rows (id, table_id, values, position, created_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING ` + rowColumns
	out, err := scanRow(tx.QueryRowContext(dbctx, q, id, r.TableId, emptyIfBlank(r.Values), r.Position, r.CreatedBy))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateRowWithLinks err: %+v", err)
		return nil, err
	}
	// No one else can see the new row to lock it, so it may come first.
	live, err := lockLinked(dbctx, tx, changes)
	if err == nil {
		_, _, err = changeLinks(dbctx, tx, changes, live, maxPerRow)
	}
	if err != nil {
		if !errors.Is(err, ErrTooManyLinks) {
			helpers.LogErrorWithContext(ctx, "models/CreateRowWithLinks links err: %+v", err)
		}
		return nil, err
	}
	return out, tx.Commit()
}

func emptyIfBlank(values string) string {
	if values == "" {
		return "{}"
	}
	return values
}

// CellsOf returns the rows of a table among ids that haven't been deleted,
// in no particular order, each with only its id and its cells keys.
func CellsOf(ctx context.Context, tableId uuid.UUID, ids []uuid.UUID, keys []string) ([]*Row, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	// The row's values read once: each -> reads all of them again.
	const q = `SELECT id, COALESCE((SELECT jsonb_object_agg(e.key, e.value) FROM jsonb_each(values) e WHERE e.key = ANY($3)), '{}'::jsonb)
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
		if err := rows.Scan(&r.Id, &r.Values); err != nil {
			return nil, err
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
		WHERE table_id=$1 AND deleted_at IS NULL ORDER BY position ASC, created_at ASC LIMIT $2 OFFSET $3`, tableId, limit, offset)
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
		ORDER BY position ASC, created_at ASC LIMIT $4`
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
	const q = `INSERT INTO data_table_fields (id, table_id, name, type, config, position) VALUES ($1,$2,$3,$4,$5,$6)`
	for _, f := range fields {
		if _, err := tx.ExecContext(dbctx, q, f.Id, f.TableId, f.Name, f.Type, f.Config, f.Position); err != nil {
			helpers.LogErrorWithContext(ctx, "models/CreateFieldsWithIDs err: %+v", err)
			return err
		}
	}
	return tx.Commit()
}

// DeleteFieldPair soft-deletes a relation field and the one showing its links
// from the other table, and the links it made, in one transaction. The other
// one may be gone already.
func DeleteFieldPair(ctx context.Context, a, b *Field) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	const q = `UPDATE data_table_fields SET deleted_at=NOW(), updated_at=NOW() WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL`
	res, err := tx.ExecContext(dbctx, q, a.Id, a.TableId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteFieldPair err: %+v", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if b != nil {
		if _, err := tx.ExecContext(dbctx, q, b.Id, b.TableId); err != nil {
			helpers.LogErrorWithContext(ctx, "models/DeleteFieldPair other err: %+v", err)
			return err
		}
	}
	if _, err := tx.ExecContext(dbctx, `DELETE FROM data_table_links WHERE field_id = $1`, a.Id); err != nil {
		return err
	}
	return tx.Commit()
}

// vacuumTimeout is how long a vacuum of the links may take.
const vacuumTimeout = 10 * time.Minute

// LinksChanged is how many links have been made and removed in all, as
// Postgres has counted them: it only ever grows, until its counts are reset.
func LinksChanged(ctx context.Context) (int64, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var n int64
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, `SELECT COALESCE(SUM(n_tup_ins + n_tup_del), 0)
		FROM pg_stat_user_tables WHERE relname = 'data_table_links'`).Scan(&n)
	return n, err
}

// VacuumLinks vacuums data_table_links, so counts and reads of it take the
// index alone, unless another vacuum of it is running.
func VacuumLinks(ctx context.Context) error {
	dbctx, cancel := context.WithTimeout(ctx, vacuumTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, `VACUUM (ANALYZE, SKIP_LOCKED) data_table_links`)
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
