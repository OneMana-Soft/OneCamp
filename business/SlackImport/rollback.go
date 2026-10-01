package business

import (
	"context"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/google/uuid"
)

// RollbackImport soft-deletes everything the import created. Order
// matters: comments before posts/chats before channels (so foreign-key
// checks don't fail), users last and only when they were created by
// this import.
//
// Three message-bearing tables now: posts (channels), chats (DMs/MPIMs),
// comments (thread replies on either). The id_map records both posts
// and chats under entity_type='message', distinguished only by which
// table physically contains the row. We try posts first, then chats,
// for each id; the WHERE deleted_at IS NULL clause keeps the second
// query a no-op if the row was in the first table.
//
// We never hard-delete. The id_map stays so a re-run (or a second
// attempt at rollback) is a no-op.
//
// Status precondition: the job must already be in a terminal state
// (completed/failed/cancelled/rolled_back). Rolling back while workers
// are still mid-write would race the soft-delete against fresh inserts
// and leak rows. Operators should call CancelImport first, then
// rollback once the cancellation propagates.
func RollbackImport(ctx context.Context, jobId uuid.UUID) error {
	job, err := importModels.GetJob(ctx, jobId)
	if err != nil {
		return err
	}

	switch job.Status {
	case importModels.StatusCompleted, importModels.StatusFailed,
		importModels.StatusCancelled, importModels.StatusRolledBack:
		// Allowed to roll back. Rolled-back is a no-op (idempotent).
	default:
		return fmt.Errorf("cannot roll back job in status %s; cancel first if it is still running", job.Status)
	}

	// Comments first.
	if err := softDelete(ctx, jobId, importModels.EntityComment, "comments"); err != nil {
		return err
	}

	// Posts (channel messages) and chats (DM/MPIM messages). Both share
	// entity_type=message in id_map; we try each table and the per-row
	// guard ensures only the physical owner is touched.
	if err := softDelete(ctx, jobId, importModels.EntityMessage, "posts"); err != nil {
		return err
	}
	if err := softDelete(ctx, jobId, importModels.EntityMessage, "chats"); err != nil {
		return err
	}

	// Attachments.
	if err := softDelete(ctx, jobId, importModels.EntityFile, "attachments"); err != nil {
		return err
	}

	// Channels (only those still empty after the above; if a non-imported
	// human posted into the imported channel, leave it alone).
	if err := softDeleteEmptyChannels(ctx, jobId); err != nil {
		return err
	}

	// External users created by this import. Only those with no other
	// activity outside this import.
	if err := softDeleteExternalUsers(ctx, jobId); err != nil {
		return err
	}

	// Reclaim MinIO storage immediately. Rollback means the operator
	// has decided this import isn't going to be retried; keeping the
	// staged ZIP would just waste space. The cleanup loop's
	// retention window doesn't apply to rolled-back jobs.
	if err := DeleteStagedZip(ctx, jobId); err != nil {
		// Non-fatal — rollback succeeded, the staged file just lingers
		// briefly. The hourly cleanup loop will pick it up next tick
		// because the rolled_back status hits the same retention check.
		helpers.LogWarnWithContext(ctx,
			"SlackImport rollback could not delete staged zip for %s: %+v",
			jobId, err)
	}

	return importModels.UpdateStatus(ctx, jobId, importModels.StatusRolledBack,
		strPtr("rolled_back"), strPtr("rolled back by operator"))
}

// softDelete sets deleted_at = NOW() on the imported rows of one table.
// Generic across posts/comments/attachments because they all share the
// same soft-delete idiom.
//
// IMPORTANT: only entities physically created by this import are
// touched (created_by_this_import = true). Entities the import merely
// re-mapped from a prior import — same workspace, same Slack id — are
// left intact, otherwise rolling back a re-import would wipe the
// original import's content too.
func softDelete(ctx context.Context, importId uuid.UUID, entityType, table string) error {
	entries, err := importModels.IdMappingsByTypeOwned(ctx, importId, entityType)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	const batchSize = 500
	for i := 0; i < len(entries); i += batchSize {
		end := i + batchSize
		if end > len(entries) {
			end = len(entries)
		}
		batch := entries[i:end]

		// Build IN-list of placeholders.
		placeholders := make([]string, 0, len(batch))
		args := make([]interface{}, 0, len(batch))
		for j, e := range batch {
			placeholders = append(placeholders, fmt.Sprintf("$%d", j+1))
			args = append(args, e.OnecampUUID)
		}
		query := fmt.Sprintf(`
			UPDATE %s SET deleted_at = NOW(), updated_at = NOW()
			WHERE id IN (%s) AND deleted_at IS NULL`,
			tableNameWhitelist(table),
			strings.Join(placeholders, ","))

		if _, err := importModels.Exec(ctx, query, args...); err != nil {
			return fmt.Errorf("rollback %s batch %d: %w", table, i, err)
		}
	}
	helpers.LogInfoWithContext(ctx,
		"SlackImport rollback soft-deleted %d rows from %s for job %s",
		len(entries), table, importId)
	return nil
}

// softDeleteEmptyChannels deletes channels created by this import that
// have zero non-imported posts. Restricted to channels physically
// created by THIS import (created_by_this_import=true) so a re-import
// rollback doesn't touch channels owned by an earlier import.
func softDeleteEmptyChannels(ctx context.Context, importId uuid.UUID) error {
	entries, err := importModels.IdMappingsByTypeOwned(ctx, importId, importModels.EntityChannel)
	if err != nil {
		return err
	}
	for _, e := range entries {
		var nonImportedCount int
		row, err := importModels.ExecQueryRow(ctx, `
			SELECT COUNT(*)
			FROM posts p
			LEFT JOIN slack_import_id_map m
			    ON m.import_id = $1 AND m.entity_type = 'message' AND m.onecamp_uuid = p.id
			WHERE p.post_channel = $2
			  AND p.deleted_at IS NULL
			  AND m.onecamp_uuid IS NULL`, importId, e.OnecampUUID)
		if err != nil {
			return err
		}
		if err := row.Scan(&nonImportedCount); err != nil {
			return err
		}
		if nonImportedCount > 0 {
			helpers.LogInfoWithContext(ctx,
				"SlackImport rollback keeping channel %s (has %d non-imported posts)",
				e.OnecampUUID, nonImportedCount)
			continue
		}
		if _, err := importModels.Exec(ctx, `
			UPDATE channels SET deleted_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND deleted_at IS NULL`, e.OnecampUUID); err != nil {
			return err
		}
	}
	return nil
}

// softDeleteExternalUsers tombstones imported external users that have
// no posts/chats/comments outside this import. Real (matched-by-email)
// users are never touched. Re-import safety: only users physically
// created by THIS import (created_by_this_import=true) are candidates.
//
// Cross-import safety: even when a user was created by this import, we
// keep them around if a later import has linked content to them via
// slack_workspace_id_map. Tombstoning a user who's authoring messages
// in another active import would orphan that content.
func softDeleteExternalUsers(ctx context.Context, importId uuid.UUID) error {
	entries, err := importModels.IdMappingsByTypeOwned(ctx, importId, importModels.EntityUser)
	if err != nil {
		return err
	}
	for _, e := range entries {
		var refsExternal int
		row, err := importModels.ExecQueryRow(ctx, `
			SELECT COUNT(*) FROM users WHERE id = $1 AND is_external = true`,
			e.OnecampUUID)
		if err != nil {
			return err
		}
		if err := row.Scan(&refsExternal); err != nil {
			return err
		}
		if refsExternal == 0 {
			continue // real user, skip
		}

		// Skip if any other import maps to this same OneCamp user. The
		// external user is shared and removing it would orphan content
		// in the other import.
		var sharedCount int
		row, err = importModels.ExecQueryRow(ctx, `
			SELECT COUNT(*) FROM slack_import_id_map
			WHERE onecamp_uuid = $1 AND import_id <> $2 AND entity_type = 'user'`,
			e.OnecampUUID, importId)
		if err != nil {
			return err
		}
		if err := row.Scan(&sharedCount); err != nil {
			return err
		}
		if sharedCount > 0 {
			continue
		}

		// Skip if the user has authored content (post/chat/comment) we
		// did NOT create in this import. If they've been active beyond
		// our import we leave them in place.
		var foreignAuthored int
		row, err = importModels.ExecQueryRow(ctx, `
			SELECT
				(SELECT COUNT(*) FROM posts WHERE created_by = $1 AND id NOT IN (
					SELECT onecamp_uuid FROM slack_import_id_map
					WHERE import_id = $2 AND entity_type = 'message'))
				+
				(SELECT COUNT(*) FROM chats WHERE created_by = $1 AND id NOT IN (
					SELECT onecamp_uuid FROM slack_import_id_map
					WHERE import_id = $2 AND entity_type = 'message'))
				+
				(SELECT COUNT(*) FROM comments WHERE created_by = $1 AND id NOT IN (
					SELECT onecamp_uuid FROM slack_import_id_map
					WHERE import_id = $2 AND entity_type = 'comment'))`,
			e.OnecampUUID, importId)
		if err != nil {
			return err
		}
		if err := row.Scan(&foreignAuthored); err != nil {
			return err
		}
		if foreignAuthored > 0 {
			continue
		}

		if _, err := importModels.Exec(ctx, `
			UPDATE users SET deleted_at = NOW(), updated_at = NOW()
			WHERE id = $1`, e.OnecampUUID); err != nil {
			return err
		}
	}
	return nil
}

// tableNameWhitelist defends against SQL injection on the table name we
// stitch into the query. Only known tables are allowed; anything else
// panics so a future caller cannot accidentally enable injection.
func tableNameWhitelist(name string) string {
	switch name {
	case "posts", "comments", "attachments", "channels", "chats", "users":
		return name
	}
	panic("rollback: refusing to interpolate unknown table " + name)
}
