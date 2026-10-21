package models

// What the import screens show of many jobs at once (backend audit U1b): the
// lists read five statements a job.

import (
	"context"
	"encoding/json"

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

// RecordMapping records what an import made (createdHere) or found for a
// source id, in its own map and in the workspace's, in one statement where
// UpsertIdMappingWithOwnership and UpsertWorkspaceMapping were two. Both are
// idempotent.
func RecordMapping(ctx context.Context, importId uuid.UUID, provider, workspace, entityType, sourceId string,
	onecampUUID uuid.UUID, parentSourceId *string, metadata json.RawMessage, createdHere bool) error {
	_, err := pgexec.Exec(ctx, `
		WITH mapped AS (
			INSERT INTO import_id_map
			    (import_id, entity_type, source_id, onecamp_uuid, parent_source_id, metadata, created_by_this_import)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (import_id, entity_type, source_id) DO NOTHING
		)
		INSERT INTO import_workspace_id_map
		    (provider, source_workspace_name, entity_type, source_id, onecamp_uuid, origin_import_id)
		SELECT $8::varchar, $9::varchar, $2, $3, $4, $1
		ON CONFLICT (provider, source_workspace_name, entity_type, source_id) DO NOTHING`,
		importId, entityType, sourceId, onecampUUID, parentSourceId, metadata, createdHere, provider, workspace)
	return err
}
