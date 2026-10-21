package business

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
	"github.com/lib/pq"
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

	// All of it in one transaction with the move to rolled_back, so a step
	// that fails leaves none of it done, rather than the tasks gone and the
	// import showing as before; and not tied to the request, which a client
	// that stops waiting would end half way through a large one, every time.
	ctx = context.WithoutCancel(ctx)
	err = importModels.RollBack(ctx, jobId, func(tx *sql.Tx) error {
		// Task-shaped rollback order: comments → subtasks → tasks →
		// attachments → projects (only if no non-imported tasks survive)
		// → teams (only if empty).
		if err := softDelete(ctx, tx, jobId, importModels.EntityComment, "comments"); err != nil {
			return err
		}
		if err := softDelete(ctx, tx, jobId, importModels.EntitySubtask, "tasks"); err != nil {
			return err
		}
		if err := softDelete(ctx, tx, jobId, importModels.EntityTask, "tasks"); err != nil {
			return err
		}
		if err := deleteUnusedFields(ctx, tx, jobId); err != nil {
			return err
		}
		if err := softDelete(ctx, tx, jobId, importModels.EntityFile, "attachments"); err != nil {
			return err
		}
		if err := softDeleteEmptyProjects(ctx, tx, jobId); err != nil {
			return err
		}
		if err := softDeleteEmptyTeams(ctx, tx, jobId); err != nil {
			return err
		}
		if err := softDelete(ctx, tx, jobId, importModels.EntityTable, "data_tables"); err != nil {
			return err
		}
		return softDeleteExternalUsers(ctx, tx, jobId)
	})
	if err != nil {
		return fmt.Errorf("the rollback stopped and took nothing away: %w", err)
	}
	// The docs live in the graph, outside the transaction: they go to the
	// trash once the rest is done, where their owner can still restore them.
	// What can't be moved stays, and rolling back again tries it again.
	if left, err := trashImportedDocs(ctx, job); err != nil || left > 0 {
		helpers.LogErrorWithContext(ctx, "Import rollback of %s left %d docs out of the trash: %+v", jobId, left, err)
		return fmt.Errorf("the import was rolled back, but %d of its docs could not be moved to the trash; roll it back again to try them again", left)
	}

	if err := DeleteStagedZip(ctx, jobId); err != nil {
		helpers.LogWarnWithContext(ctx,
			"Import rollback could not delete staged zip for %s: %+v", jobId, err)
	}
	return nil
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
func softDelete(ctx context.Context, tx *sql.Tx, importId uuid.UUID, entityType, table string) error {
	// One statement for all of them, where it read every mapping row and
	// wrote 500 at a time.
	res, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %s SET %s
		WHERE id IN (
			SELECT onecamp_uuid FROM import_id_map
			WHERE import_id = $1 AND entity_type = $2 AND created_by_this_import = true)
		  AND deleted_at IS NULL`, tableNameWhitelist(table), markDeleted(table)), importId, entityType)
	if err != nil {
		return fmt.Errorf("rollback %s: %w", table, err)
	}
	n, _ := res.RowsAffected()
	helpers.LogInfoWithContext(ctx,
		"Import rollback soft-deleted %d rows from %s for job %s",
		n, table, importId)
	return nil
}

// deleteUnusedFields takes away the custom fields this import made, once its
// tasks are gone, unless a task still in use has a value of one: a field
// someone has started using stays.
func deleteUnusedFields(ctx context.Context, tx *sql.Tx, importId uuid.UUID) error {
	entries, err := importModels.IdMappingsByTypeOwned(ctx, importId, importModels.EntityField)
	if err != nil || len(entries) == 0 {
		return err
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.OnecampUUID.String())
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM task_fields f
		WHERE f.id = ANY($1::uuid[])
		  AND NOT EXISTS (
		      SELECT 1 FROM task_field_values v JOIN tasks t ON t.id = v.task_uuid
		      WHERE v.field_id = f.id AND t.deleted_at IS NULL)`, pq.Array(ids)); err != nil {
		return fmt.Errorf("rollback task fields: %w", err)
	}
	return nil
}

// softDeleteEmptyProjects deletes projects created by this import that
// have zero non-imported tasks. Real-user activity inside an imported
// project keeps it alive.
func softDeleteEmptyProjects(ctx context.Context, tx *sql.Tx, importId uuid.UUID) error {
	// One statement, where it was two a project.
	_, err := tx.ExecContext(ctx, `
		UPDATE projects p SET deleted_at = NOW(), updated_at = NOW()
		WHERE p.id IN (
			SELECT onecamp_uuid FROM import_id_map
			WHERE import_id = $1 AND entity_type = 'project' AND created_by_this_import = true)
		  AND p.deleted_at IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM tasks t
			LEFT JOIN import_id_map m
			    ON m.import_id = $1 AND m.entity_type IN ('task','subtask') AND m.onecamp_uuid = t.id
			WHERE t.project_id = p.id AND t.deleted_at IS NULL AND m.onecamp_uuid IS NULL)`, importId)
	return err
}

// softDeleteEmptyTeams tombstones teams created by this import that
// have zero non-imported projects.
func softDeleteEmptyTeams(ctx context.Context, tx *sql.Tx, importId uuid.UUID) error {
	// One statement, where it was two a team.
	_, err := tx.ExecContext(ctx, `
		UPDATE teams tm SET deleted_at = NOW(), updated_at = NOW()
		WHERE tm.id IN (
			SELECT onecamp_uuid FROM import_id_map
			WHERE import_id = $1 AND entity_type = 'team' AND created_by_this_import = true)
		  AND tm.deleted_at IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM projects p
			LEFT JOIN import_id_map m
			    ON m.import_id = $1 AND m.entity_type = 'project' AND m.onecamp_uuid = p.id
			WHERE p.team_id = tm.id AND p.deleted_at IS NULL AND m.onecamp_uuid IS NULL)`, importId)
	return err
}

// softDeleteExternalUsers tombstones imported external users that have
// no posts/chats/comments/tasks outside this import.
func softDeleteExternalUsers(ctx context.Context, tx *sql.Tx, importId uuid.UUID) error {
	// One statement, where it was three a person: those this import made who
	// wrote nothing it didn't bring and no other import maps.
	_, err := tx.ExecContext(ctx, `
		UPDATE users u SET deleted_at = NOW(), updated_at = NOW()
		WHERE u.id IN (
			SELECT onecamp_uuid FROM import_id_map
			WHERE import_id = $1 AND entity_type = 'user' AND created_by_this_import = true)
		  AND u.deleted_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM tasks x WHERE x.created_by = u.id AND x.id NOT IN (
			SELECT onecamp_uuid FROM import_id_map WHERE import_id = $1 AND entity_type IN ('task','subtask')))
		  AND NOT EXISTS (SELECT 1 FROM comments x WHERE x.created_by = u.id AND x.id NOT IN (
			SELECT onecamp_uuid FROM import_id_map WHERE import_id = $1 AND entity_type = 'comment'))
		  AND NOT EXISTS (SELECT 1 FROM posts x WHERE x.created_by = u.id AND x.id NOT IN (
			SELECT onecamp_uuid FROM import_id_map WHERE import_id = $1 AND entity_type = 'message'))
		  AND NOT EXISTS (SELECT 1 FROM chats x WHERE x.created_by = u.id AND x.id NOT IN (
			SELECT onecamp_uuid FROM import_id_map WHERE import_id = $1 AND entity_type = 'message'))
		  AND NOT EXISTS (SELECT 1 FROM import_id_map o
			WHERE o.onecamp_uuid = u.id AND o.import_id <> $1 AND o.entity_type = 'user')`, importId)
	return err
}

// markDeleted is the SET clause that marks a row deleted. attachments has no
// updated_at, so setting it failed the statement, and with it the rollback of
// every import that brought a file, after its tasks were already gone.
func markDeleted(table string) string {
	if table == "attachments" {
		return "deleted_at = NOW()"
	}
	return "deleted_at = NOW(), updated_at = NOW()"
}

// tableNameWhitelist is a tiny defence against table-name injection.
// Every caller passes a literal string we control, but having an
// explicit allow-list documents the rule.
func tableNameWhitelist(t string) string {
	switch t {
	case "tasks", "comments", "attachments", "projects", "teams",
		"posts", "chats", "channels", "users", "data_tables":
		return t
	default:
		// This should never trigger; if it does, the caller has a bug.
		// Returning a safe sentinel makes the resulting query fail loud.
		return "__INVALID_TABLE__"
	}
}

// trashDoc puts a doc and the pages under it in the trash as one delete; a
// seam for tests.
var trashDoc = docBusiness.TrashSubtree

// trashImportedDocs puts the docs the import made in the trash, a tree as one
// delete (roots come first, so a page goes with the page above it), by whoever
// ran the import, and says how many it couldn't move. A doc already in the
// trash, or gone, is done.
func trashImportedDocs(ctx context.Context, job *importModels.Job) (int, error) {
	entries, err := importModels.IdMappingsByTypeOwned(ctx, job.Id, importModels.EntityDoc)
	if err != nil || len(entries) == 0 {
		return len(entries), err
	}
	if job.TriggeredBy == nil {
		return len(entries), errors.New("the import names nobody who ran it")
	}
	by, err := userDomain.GetDgraphUserInfoByUUID(ctx, job.TriggeredBy.String())
	if err != nil || by == nil {
		return len(entries), fmt.Errorf("whoever ran it could not be read: %w", err)
	}
	left := 0
	for _, e := range entries {
		_, err := trashDoc(ctx, e.OnecampUUID.String(), by.Uid, job.TriggeredBy.String(), by.EmailID)
		if err != nil && !errors.Is(err, docBusiness.ErrDocDeleted) && !errors.Is(err, docBusiness.ErrNotFound) {
			helpers.LogWarnWithContext(ctx, "Import rollback could not move doc %s to the trash: %+v", e.OnecampUUID, err)
			left++
		}
	}
	return left, nil
}
