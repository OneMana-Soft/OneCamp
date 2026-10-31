package models

// What the import screens show of many jobs at once (backend audit U1b): the
// lists read five statements a job.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// JobCount is what a jobs list shows of an import's progress.
type JobCount struct {
	ChunksTotal, ChunksDone, ChunksFailed, ItemsImported, ErrorsTotal int
}

// JobCounts is each import's chunk, item and error counts, for any number
// of imports in one statement.
func JobCounts(ctx context.Context, jobIds []uuid.UUID) (map[uuid.UUID]JobCount, error) {
	out := make(map[uuid.UUID]JobCount, len(jobIds))
	if len(jobIds) == 0 {
		return out, nil
	}
	type row struct {
		id uuid.UUID
		c  JobCount
	}
	rows, err := pgexec.Query(ctx, `
		SELECT j.id, COALESCE(c.total, 0), COALESCE(c.done, 0), COALESCE(c.failed, 0),
		       COALESCE(c.items, 0), COALESCE(e.n, 0)
		FROM unnest($1::uuid[]) AS j(id)
		LEFT JOIN (
			SELECT import_id, COUNT(*) AS total,
			       COUNT(*) FILTER (WHERE status = 'done') AS done,
			       COUNT(*) FILTER (WHERE status = 'failed') AS failed,
			       SUM(items_done) AS items
			FROM import_chunks WHERE import_id = ANY($1::uuid[]) GROUP BY import_id
		) c ON c.import_id = j.id
		LEFT JOIN (
			SELECT import_id, COUNT(*) AS n
			FROM import_errors WHERE import_id = ANY($1::uuid[]) GROUP BY import_id
		) e ON e.import_id = j.id`, func(r pgexec.Row) (x row, err error) {
		err = r.Scan(&x.id, &x.c.ChunksTotal, &x.c.ChunksDone, &x.c.ChunksFailed, &x.c.ItemsImported, &x.c.ErrorsTotal)
		return
	}, pq.Array(jobIds))
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.id] = x.c
	}
	return out, nil
}

// JobMappings is each import's status and priority mappings (source ->
// target), in two statements for any number of imports.
func JobMappings(ctx context.Context, jobIds []uuid.UUID) (status, priority map[uuid.UUID]map[string]string, err error) {
	if status, err = mappingsOf(ctx, `SELECT import_id, source_status, target_status FROM import_status_mappings WHERE import_id = ANY($1::uuid[])`, jobIds); err != nil {
		return nil, nil, err
	}
	if priority, err = mappingsOf(ctx, `SELECT import_id, source_priority, target_priority FROM import_priority_mappings WHERE import_id = ANY($1::uuid[])`, jobIds); err != nil {
		return nil, nil, err
	}
	return status, priority, nil
}

func mappingsOf(ctx context.Context, q string, jobIds []uuid.UUID) (map[uuid.UUID]map[string]string, error) {
	out := make(map[uuid.UUID]map[string]string, len(jobIds))
	if len(jobIds) == 0 {
		return out, nil
	}
	type row struct {
		id       uuid.UUID
		src, tgt string
	}
	rows, err := pgexec.Query(ctx, q, func(r pgexec.Row) (x row, err error) {
		err = r.Scan(&x.id, &x.src, &x.tgt)
		return
	}, pq.Array(jobIds))
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		if out[x.id] == nil {
			out[x.id] = map[string]string{}
		}
		out[x.id][x.src] = x.tgt
	}
	return out, nil
}

// RecordMapping is RecordMappings for one source id. Its error names what it
// couldn't remember, for the item's own.
func RecordMapping(ctx context.Context, importId uuid.UUID, provider, workspace, entityType, sourceId string,
	onecampUUID uuid.UUID, parentSourceId *string, metadata json.RawMessage, createdHere bool) error {
	if err := RecordMappings(ctx, importId, provider, workspace, entityType, []Mapped{{
		SourceID: sourceId, OnecampUUID: onecampUUID, ParentSourceID: parentSourceId, Metadata: metadata, CreatedHere: createdHere,
	}}); err != nil {
		return fmt.Errorf("remember %s %s: %w", entityType, sourceId, err)
	}
	return nil
}

// Mapped is a source id and what an import made (CreatedHere) or found for
// it, with its parent's source id and its metadata when it has them.
type Mapped struct {
	SourceID       string
	OnecampUUID    uuid.UUID
	ParentSourceID *string
	Metadata       json.RawMessage
	CreatedHere    bool
}

// RecordMappings records what an import made or found for source ids of one
// entity type, in its own map and, when it names its workspace, in the
// workspace's, so the next import of the workspace skips them: one statement
// where an upsert into each map was two a row. Both are idempotent.
func RecordMappings(ctx context.Context, importId uuid.UUID, provider, workspace, entityType string, rows []Mapped) error {
	if len(rows) == 0 {
		return nil
	}
	sources := make([]string, len(rows))
	ids := make([]string, len(rows))
	parents := make([]sql.NullString, len(rows))
	metadata := make([]sql.NullString, len(rows))
	made := make([]bool, len(rows))
	for i, r := range rows {
		sources[i], ids[i], made[i] = r.SourceID, r.OnecampUUID.String(), r.CreatedHere
		if r.ParentSourceID != nil {
			parents[i] = sql.NullString{String: *r.ParentSourceID, Valid: true}
		}
		if len(r.Metadata) > 0 {
			metadata[i] = sql.NullString{String: string(r.Metadata), Valid: true}
		}
	}
	_, err := pgexec.Exec(ctx, `
		WITH rows AS (
			SELECT * FROM unnest($3::varchar[], $4::uuid[], $5::varchar[], $6::text[], $7::boolean[])
			    AS r(source_id, onecamp_uuid, parent_source_id, metadata, made)
		), mapped AS (
			INSERT INTO import_id_map
			    (import_id, entity_type, source_id, onecamp_uuid, parent_source_id, metadata, created_by_this_import)
			SELECT $1, $2, source_id, onecamp_uuid, parent_source_id, metadata::jsonb, made FROM rows
			ON CONFLICT (import_id, entity_type, source_id) DO NOTHING
		)
		INSERT INTO import_workspace_id_map
		    (provider, source_workspace_name, entity_type, source_id, onecamp_uuid, origin_import_id)
		SELECT $8::varchar, $9::varchar, $2, source_id, onecamp_uuid, $1 FROM rows WHERE $9::varchar <> ''
		ON CONFLICT (provider, source_workspace_name, entity_type, source_id) DO NOTHING`,
		importId, entityType, pq.Array(sources), pq.Array(ids), pq.Array(parents), pq.Array(metadata), pq.Array(made), provider, workspace)
	return err
}

// CommentImport is an import that brought a comment in: the import, its
// provider, and when its first run started (At is nil when the job has no
// time at all).
type CommentImport struct {
	Comment  uuid.UUID
	Import   uuid.UUID
	Provider string
	At       *time.Time
}

// CommentImports are the imports that brought in each of comments, as their
// id maps recorded them: none for a comment no import recorded.
func CommentImports(ctx context.Context, comments []uuid.UUID) ([]CommentImport, error) {
	if len(comments) == 0 {
		return nil, nil
	}
	ids := make([]string, len(comments))
	for i, c := range comments {
		ids[i] = c.String()
	}
	return pgexec.Query(ctx, `
		SELECT m.onecamp_uuid, m.import_id, j.provider, COALESCE(j.started_at, j.created_at)
		FROM import_id_map m JOIN import_jobs j ON j.id = m.import_id
		WHERE m.entity_type = $1 AND m.onecamp_uuid = ANY($2::uuid[])`,
		func(r pgexec.Row) (CommentImport, error) {
			var c CommentImport
			var at sql.NullTime
			if err := r.Scan(&c.Comment, &c.Import, &c.Provider, &at); err != nil {
				return c, err
			}
			if at.Valid {
				c.At = &at.Time
			}
			return c, nil
		}, EntityComment, pq.Array(ids))
}
