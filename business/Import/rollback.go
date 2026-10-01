package business

import (
	"context"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// RollbackImport soft-deletes everything the import created, in
// dependency order so foreign-key checks don't fail. Idempotent.
//
// Status precondition: the job must already be terminal. Operators
// must Cancel a running import first.
//
// Slack jobs delegate to the legacy SlackImport.Rollback (channels +
// messages + DMs etc.) because the schema there is different. New
// providers (task-shaped) take the path below.
func RollbackImport(ctx context.Context, jobId uuid.UUID) error {
	job, err := importModels.GetJob(ctx, jobId)
	if err != nil {
		return err
	}
	switch job.Status {
	case importModels.StatusCompleted, importModels.StatusFailed,
		importModels.StatusCancelled, importModels.StatusRolledBack:
	default:
		return fmt.Errorf("cannot roll back job in status %s; cancel first", job.Status)
	}

	if job.Provider == importModels.ProviderSlack {
		return slackRollback(ctx, jobId)
	}

	// Task-shaped rollback order: comments → subtasks → tasks →
	// attachments → projects (only if no non-imported tasks survive)
	// → teams (only if empty).
	if err := softDelete(ctx, jobId, importModels.EntityComment, "comments"); err != nil {
		return err
	}
	if err := softDelete(ctx, jobId, importModels.EntitySubtask, "tasks"); err != nil {
		return err
	}
	if err := softDelete(ctx, jobId, importModels.EntityTask, "tasks"); err != nil {
		return err
	}
	if err := softDelete(ctx, jobId, importModels.EntityFile, "attachments"); err != nil {
		return err
	}
	if err := softDeleteEmptyProjects(ctx, jobId); err != nil {
		return err
	}
	if err := softDeleteEmptyTeams(ctx, jobId); err != nil {
		return err
	}
	if err := softDeleteExternalUsers(ctx, jobId); err != nil {
		return err
	}

	if err := DeleteStagedZip(ctx, jobId); err != nil {
		helpers.LogWarnWithContext(ctx,
			"Import rollback could not delete staged zip for %s: %+v", jobId, err)
	}
	stage := "rolled_back"
	reason := "rolled back by operator"
	return importModels.UpdateStatus(ctx, jobId,
		importModels.StatusRolledBack, &stage, &reason)
}

// slackRollback delegates to the legacy SlackImport package. We can't
// import the package here without a circular dependency, so we route
// via a small re-export below in slack_rollback_bridge.go.
func slackRollback(ctx context.Context, jobId uuid.UUID) error {
	return invokeSlackRollback(ctx, jobId)
}

// invokeSlackRollback is wired in slack_rollback_bridge.go to call into
// the legacy SlackImport business package. Defined as a variable so the
// bridge file can populate it without creating a circular import.
var invokeSlackRollback = func(ctx context.Context, jobId uuid.UUID) error {
	return fmt.Errorf("slack rollback bridge not initialised")
}

// softDelete is the generic batched UPDATE deleted_at = NOW() helper.
// Only entities physically created by THIS import (created_by_this_import
// = true) are touched. Imports that re-mapped an existing entity from a
// prior import don't tombstone it.
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
		"Import rollback soft-deleted %d rows from %s for job %s",
		len(entries), table, importId)
	return nil
}

// softDeleteEmptyProjects deletes projects created by this import that
// have zero non-imported tasks. Real-user activity inside an imported
// project keeps it alive.
func softDeleteEmptyProjects(ctx context.Context, importId uuid.UUID) error {
	entries, err := importModels.IdMappingsByTypeOwned(ctx, importId, importModels.EntityProject)
	if err != nil {
		return err
	}
	for _, e := range entries {
		var nonImportedCount int
		row, err := importModels.ExecQueryRow(ctx, `
			SELECT COUNT(*)
			FROM tasks t
			LEFT JOIN import_id_map m
			    ON m.import_id = $1
			   AND m.entity_type IN ('task','subtask')
			   AND m.onecamp_uuid = t.id
			WHERE t.project_id = $2
			  AND t.deleted_at IS NULL
			  AND m.onecamp_uuid IS NULL`, importId, e.OnecampUUID)
		if err != nil {
			return err
		}
		if err := row.Scan(&nonImportedCount); err != nil {
			return err
		}
		if nonImportedCount > 0 {
			helpers.LogInfoWithContext(ctx,
				"Import rollback keeping project %s (has %d non-imported tasks)",
				e.OnecampUUID, nonImportedCount)
			continue
		}
		if _, err := importModels.Exec(ctx, `
			UPDATE projects SET deleted_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND deleted_at IS NULL`, e.OnecampUUID); err != nil {
			return err
		}
	}
	return nil
}

// softDeleteEmptyTeams tombstones teams created by this import that
// have zero non-imported projects.
func softDeleteEmptyTeams(ctx context.Context, importId uuid.UUID) error {
	entries, err := importModels.IdMappingsByTypeOwned(ctx, importId, importModels.EntityTeam)
	if err != nil {
		return err
	}
	for _, e := range entries {
		var nonImportedCount int
		row, err := importModels.ExecQueryRow(ctx, `
			SELECT COUNT(*)
			FROM projects p
			LEFT JOIN import_id_map m
			    ON m.import_id = $1
			   AND m.entity_type = 'project'
			   AND m.onecamp_uuid = p.id
			WHERE p.team_id = $2
			  AND p.deleted_at IS NULL
			  AND m.onecamp_uuid IS NULL`, importId, e.OnecampUUID)
		if err != nil {
			return err
		}
		if err := row.Scan(&nonImportedCount); err != nil {
			return err
		}
		if nonImportedCount > 0 {
			continue
		}
		if _, err := importModels.Exec(ctx, `
			UPDATE teams SET deleted_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND deleted_at IS NULL`, e.OnecampUUID); err != nil {
			return err
		}
	}
	return nil
}

// softDeleteExternalUsers tombstones imported external users that have
// no posts/chats/comments/tasks outside this import.
func softDeleteExternalUsers(ctx context.Context, importId uuid.UUID) error {
	entries, err := importModels.IdMappingsByTypeOwned(ctx, importId, importModels.EntityUser)
	if err != nil {
		return err
	}
	for _, e := range entries {
		var refsExternal int
		row, err := importModels.ExecQueryRow(ctx, `
			SELECT
			    (SELECT COUNT(*) FROM tasks    WHERE created_by = $1 AND id NOT IN (
			        SELECT onecamp_uuid FROM import_id_map
			        WHERE import_id = $2 AND entity_type IN ('task','subtask')))
			    +
			    (SELECT COUNT(*) FROM comments WHERE created_by = $1 AND id NOT IN (
			        SELECT onecamp_uuid FROM import_id_map
			        WHERE import_id = $2 AND entity_type = 'comment'))
			    +
			    (SELECT COUNT(*) FROM posts    WHERE created_by = $1 AND id NOT IN (
			        SELECT onecamp_uuid FROM import_id_map
			        WHERE import_id = $2 AND entity_type = 'message'))
			    +
			    (SELECT COUNT(*) FROM chats    WHERE created_by = $1 AND id NOT IN (
			        SELECT onecamp_uuid FROM import_id_map
			        WHERE import_id = $2 AND entity_type = 'message'))`,
			e.OnecampUUID, importId)
		if err != nil {
			return err
		}
		if err := row.Scan(&refsExternal); err != nil {
			return err
		}
		if refsExternal > 0 {
			continue
		}
		// Cross-import safety: if any other import mapped this user, leave them.
		var otherImports int
		row, err = importModels.ExecQueryRow(ctx, `
			SELECT COUNT(*) FROM import_id_map
			WHERE onecamp_uuid = $1 AND import_id <> $2 AND entity_type = 'user'`,
			e.OnecampUUID, importId)
		if err != nil {
			return err
		}
		if err := row.Scan(&otherImports); err != nil {
			return err
		}
		if otherImports > 0 {
			continue
		}
		if _, err := importModels.Exec(ctx, `
			UPDATE users SET deleted_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND deleted_at IS NULL`, e.OnecampUUID); err != nil {
			return err
		}
	}
	return nil
}

// tableNameWhitelist is a tiny defence against table-name injection.
// Every caller passes a literal string we control, but having an
// explicit allow-list documents the rule.
func tableNameWhitelist(t string) string {
	switch t {
	case "tasks", "comments", "attachments", "projects", "teams",
		"posts", "chats", "channels", "users":
		return t
	default:
		// This should never trigger; if it does, the caller has a bug.
		// Returning a safe sentinel makes the resulting query fail loud.
		return "__INVALID_TABLE__"
	}
}
