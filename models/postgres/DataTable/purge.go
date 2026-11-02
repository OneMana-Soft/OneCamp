package models

// A table deleted for good (V-121; migration 542's purge_at): claimed as the
// delete starts, read for what its rows hold outside the tables' own, and
// removed with everything of its own, its rows a chunk at a time.

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// purgeChunk is how many of a table's rows one statement removes. Each takes
// its cell index entries, links and AI fills with it (ON DELETE CASCADE), so a
// 100,000-row table goes in bounded statements, each under BulkTimeout.
const purgeChunk = 2000

// ClaimTableForPurge claims a table in the trash for its delete for good by
// a person, at at. From then on it is out of the trash: its lists and a
// restore leave it out (DeletedTables, GetDeletedTable, RestoreTable), and
// the trash's sweep finishes a delete a stopped server left part way
// (ClaimStalePurge). It says whether it claimed it: false when the table
// isn't in the trash, or is claimed already.
func ClaimTableForPurge(ctx context.Context, id, by uuid.UUID, at time.Time) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE data_tables SET purge_at = $3, purge_by = $2
		WHERE id = $1 AND deleted_at IS NOT NULL AND purge_at IS NULL`, id, by, at)
	return n > 0, err
}

// ClaimTablesPastRetention claims for the server, at at, at most limit
// tables in the trash since before cutoff, oldest delete first, and says
// which.
func ClaimTablesPastRetention(ctx context.Context, cutoff, at time.Time, limit int) ([]uuid.UUID, error) {
	return pgexec.Query(ctx, `UPDATE data_tables SET purge_at = $2, purge_by = NULL WHERE id IN (
		SELECT id FROM data_tables WHERE deleted_at < $1 AND purge_at IS NULL
		ORDER BY deleted_at, id LIMIT $3 FOR UPDATE SKIP LOCKED)
		RETURNING id`, scanID, cutoff, at, limit)
}

// Claim is a table claimed for its delete for good, and who by: uuid.Nil is
// the server (its retention).
type Claim struct {
	ID, By uuid.UUID
}

// ClaimStalePurge claims again, at at, the oldest delete for good claimed
// before stale and never finished (a server stopped part way), and says
// which, with who started it. found is false when there's none.
func ClaimStalePurge(ctx context.Context, stale, at time.Time) (c Claim, found bool, err error) {
	return pgexec.QueryOne(ctx, `UPDATE data_tables SET purge_at = $2 WHERE id = (
		SELECT id FROM data_tables WHERE deleted_at IS NOT NULL AND purge_at < $1
		ORDER BY purge_at, id LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING id, purge_by`, func(r pgexec.Row) (c Claim, err error) {
		var by uuid.NullUUID
		err = r.Scan(&c.ID, &by)
		c.By = by.UUID
		return c, err
	}, stale, at)
}

// Holding is a row of a table deleted for good, with its table and its page
// (a doc), if it has one: what the rows hold outside the tables' own goes
// through its owners before the rows go.
type Holding struct {
	Table uuid.UUID
	Row   uuid.UUID
	Page  uuid.UUID // uuid.Nil: none
}

// HoldingsOfTables is every row of these tables, in the trash or not, with
// its table and its page.
func HoldingsOfTables(ctx context.Context, ids []uuid.UUID) ([]Holding, error) {
	var out []Holding
	err := bulk(ctx, func(ctx context.Context) (err error) {
		out, err = pgexec.Query(ctx, `SELECT table_id, id, doc_uuid FROM data_table_rows WHERE table_id = ANY($1)`,
			func(r pgexec.Row) (h Holding, err error) {
				var page uuid.NullUUID
				err = r.Scan(&h.Table, &h.Row, &page)
				h.Page = page.UUID
				return h, err
			}, ids)
		return err
	})
	return out, err
}

// ForgetRowPages unlinks the rows of these tables from pages docs' purge has
// taken, so a delete for good that takes a table's pages over several passes
// finds only the pages left (HoldingsOfTables) in the next.
func ForgetRowPages(ctx context.Context, tables, pages []uuid.UUID) error {
	if len(pages) == 0 {
		return nil
	}
	return bulk(ctx, func(ctx context.Context) error {
		_, err := pgexec.Exec(ctx, `UPDATE data_table_rows SET doc_uuid = NULL WHERE table_id = ANY($1) AND doc_uuid = ANY($2)`, tables, pages)
		return err
	})
}

// DeleteTablesForGood removes the tables of ids claimed for their delete for
// good, with everything of theirs in the tables' own store: their rows a chunk
// at a time (each chunk's change log first, which names rows without a key;
// the cell index, links and AI fills go with the rows), then the tables, whose
// fields, views, change sets, stored formulas and their marks go with them.
// Every step finds only what is left, so a delete cut short is finished by
// running it again. It says which tables it removed.
func DeleteTablesForGood(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	for {
		var n int64
		err := bulk(ctx, func(ctx context.Context) error {
			rows, err := pgexec.Query(ctx, `SELECT r.id FROM data_table_rows r JOIN data_tables t ON t.id = r.table_id
				WHERE r.table_id = ANY($1) AND t.purge_at IS NOT NULL LIMIT $2`, scanID, ids, purgeChunk)
			if err != nil || len(rows) == 0 {
				return err
			}
			if _, err := pgexec.Exec(ctx, `DELETE FROM data_table_changes WHERE row_id = ANY($1)`, rows); err != nil {
				return err
			}
			n, err = pgexec.Exec(ctx, `DELETE FROM data_table_rows WHERE id = ANY($1)`, rows)
			return err
		})
		if err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
	}
	var gone []uuid.UUID
	err := bulk(ctx, func(ctx context.Context) (err error) {
		gone, err = pgexec.Query(ctx, `DELETE FROM data_tables WHERE id = ANY($1) AND purge_at IS NOT NULL RETURNING id`, scanID, ids)
		return err
	})
	return gone, err
}

// bulk runs fn in a transaction of its own under BulkTimeout: a statement
// over a whole table's rows needs longer than the pool's timeout.
func bulk(ctx context.Context, fn func(ctx context.Context) error) error {
	return postgresInit.InTx(ctx, postgresInit.BulkTimeout, func(ctx context.Context, tx *sql.Tx) error {
		return fn(pgexec.WithTx(ctx, tx))
	})
}
