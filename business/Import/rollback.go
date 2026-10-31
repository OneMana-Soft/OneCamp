package business

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	archiveBusiness "github.com/akashc777/OneCamp/business/Archive"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/pgexec"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	softDeleteModel "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
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
		if err := deleteUnusedMade(ctx, tx, jobId, importModels.EntityField); err != nil {
			return err
		}
		if err := deleteUnusedMade(ctx, tx, jobId, importModels.EntityCycle); err != nil {
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
	// Boards and lists read the graph, and search keeps its own copy: what the
	// transaction deleted goes from them too. What can't stays, and rolling
	// back again tries it again.
	if left, err := unmakeImported(ctx, job); err != nil || left > 0 {
		helpers.LogErrorWithContext(ctx, "Import rollback of %s left %d of its tasks, projects and teams on the graph or in search: %+v", jobId, left, err)
		if err != nil {
			return fmt.Errorf("the import was rolled back, but its tasks, projects and teams could not all be taken off its boards, lists and search; roll it back again to try them again")
		}
		return fmt.Errorf("the import was rolled back, but %d of its tasks, projects and teams are still on its boards, lists or search; roll it back again to try them again", left)
	}
	// The docs live in the graph, outside the transaction: they go to the
	// trash once the rest is done, where their owner can still restore them.
	// What can't be moved stays, and rolling back again tries it again.
	if left, err := trashImportedDocs(ctx, job); err != nil || left > 0 {
		helpers.LogErrorWithContext(ctx, "Import rollback of %s left %d docs out of the trash: %+v", jobId, left, err)
		return fmt.Errorf("the import was rolled back, but %d of its docs could not be moved to the trash; roll it back again to try them again", left)
	}
	// So do its boards (their pictures went with the attachments above).
	if left := deleteImportedBoards(ctx, job); left > 0 {
		return fmt.Errorf("the import was rolled back, but %d of its boards could not be deleted; roll it back again to try them again", left)
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

// unusedMade is how a rollback takes away each kind of thing an import makes
// in a project, once its tasks are gone, unless a task still in use holds it:
// a field or cycle someone has started using stays. It goes softly, as a
// person's delete takes one (SoftDelete, V-78): its task values or task links
// stay, so its restore brings them back with it.
var unusedMade = map[string]struct {
	kind   softDeleteModel.Kind
	unused string
}{
	importModels.EntityField: {softDeleteModel.Field, `NOT EXISTS (
		SELECT 1 FROM task_field_values v JOIN tasks t ON t.id = v.task_uuid
		WHERE v.field_id = task_fields.id AND t.deleted_at IS NULL)`},
	importModels.EntityCycle: {softDeleteModel.Cycle, `NOT EXISTS (
		SELECT 1 FROM task_cycles tc JOIN tasks t ON t.id = tc.task_uuid
		WHERE tc.cycle_id = project_cycles.id AND t.deleted_at IS NULL)`},
}

// deleteUnusedMade takes away, in the rollback's transaction, the fields or
// cycles (unusedMade) this import made that no task in use holds.
func deleteUnusedMade(ctx context.Context, tx *sql.Tx, importId uuid.UUID, entityType string) error {
	entries, err := importModels.IdMappingsByTypeOwned(ctx, importId, entityType)
	if err != nil || len(entries) == 0 {
		return err
	}
	ids := make([]uuid.UUID, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.OnecampUUID)
	}
	made := unusedMade[entityType]
	if _, err := softDeleteModel.DeleteAll(pgexec.WithTx(ctx, tx), made.kind, ids, uuid.Nil, made.unused); err != nil {
		return fmt.Errorf("rollback %s: %w", entityType, err)
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

// deleteImportedBoards deletes the boards the import made, as their owner
// deletes one, and says how many it couldn't. A board deleted already is done.
func deleteImportedBoards(ctx context.Context, job *importModels.Job) int {
	entries, err := importModels.IdMappingsByTypeOwned(ctx, job.Id, importModels.EntityBoard)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Import rollback of %s could not list its boards: %+v", job.Id, err)
		return 1
	}
	left := 0
	for _, e := range entries {
		if err := deleteBoard(ctx, e.OnecampUUID.String()); err != nil {
			helpers.LogWarnWithContext(ctx, "Import rollback could not delete board %s: %+v", e.OnecampUUID, err)
			left++
		}
	}
	return left
}

// unmakeImported takes what the rollback's transaction deleted off the graph
// and search, kind by kind (subtasks before their tasks, tasks before their
// projects, projects before their teams), and says how many it couldn't.
// Tasks and subtasks go in bulk, as an archive run takes them; projects and
// teams, which an import makes few of, each by its own delete. It reads as
// whoever ran the import, and as the system: a task only a few may see is the
// import's too. A bulk import's context: none of it raises an event or a
// notification.
func unmakeImported(ctx context.Context, job *importModels.Job) (int, error) {
	kinds := []unmaker{unmakeSubtasks, unmakeTasks, unmakeProjects, unmakeTeams}
	made := make([][]uuid.UUID, len(kinds))
	some := false
	for i, k := range kinds {
		ids, err := importModels.MadeAndDeleted(ctx, job.Id, k.entityType())
		if err != nil {
			return 0, fmt.Errorf("list its %ss: %w", k.entityType(), err)
		}
		made[i], some = ids, some || len(ids) > 0
	}
	if !some {
		return 0, nil
	}
	by, err := importRunner(ctx, job)
	if err != nil {
		return 0, err
	}
	ctx = importContext(ctx)
	left := 0
	for i, k := range kinds {
		left += k.run(ctx, made[i], by)
	}
	return left, nil
}

// unmaker is one kind of what an import makes, as a rollback takes it off the
// graph and search (unmaking).
type unmaker interface {
	entityType() string
	run(ctx context.Context, ids []uuid.UUID, by *dgraphStruct.DgraphUser) (left int)
}

// unmaking is one kind of unmaker: live reads the nodes the graph still has
// live of some of its ids (none for one the graph never had), and archive
// takes those off the graph and search, saying how many it couldn't.
type unmaking[N any] struct {
	entity  string
	live    func(ctx context.Context, ids []uuid.UUID, by *dgraphStruct.DgraphUser) (map[uuid.UUID]N, error)
	archive func(ctx context.Context, nodes map[uuid.UUID]N) (left int)
}

// unmakeBatch is how many of a kind one read of the graph takes.
const unmakeBatch = 500

func (u unmaking[N]) entityType() string { return u.entity }

// run takes those of ids the graph still has live off it and search, reading
// them as by, and says how many it couldn't.
func (u unmaking[N]) run(ctx context.Context, ids []uuid.UUID, by *dgraphStruct.DgraphUser) int {
	left := 0
	for batch := range slices.Chunk(ids, unmakeBatch) {
		nodes, err := u.live(ctx, batch, by)
		if err != nil {
			helpers.LogWarnWithContext(ctx, "Import rollback could not read %d %ss in the graph: %+v", len(batch), u.entity, err)
			left += len(batch)
			continue
		}
		left += u.archive(ctx, nodes)
	}
	return left
}

var (
	unmakeSubtasks = unmaking[*dgraphStruct.DgraphTask]{importModels.EntitySubtask, liveTasks, archiveTasks}
	unmakeTasks    = unmaking[*dgraphStruct.DgraphTask]{importModels.EntityTask, liveTasks, archiveTasks}
	unmakeProjects = unmaking[*dgraphStruct.DgraphProject]{importModels.EntityProject, liveProjects, oneByOne(importModels.EntityProject, projectBusiness.ArchiveProjectByProjectUUID)}
	unmakeTeams    = unmaking[*dgraphStruct.DgraphTeam]{importModels.EntityTeam, liveTeams, oneByOne(importModels.EntityTeam, teamBusiness.ArchiveTeamByTeamUUID)}
)

// archiveTasks takes tasks off the graph and search as an archive run does:
// one write of the graph for them all, and one update of search for them and
// their comments, files and AI entries. A task's own delete is a write and an
// update for each, and a large import's rollback ran past the request's time.
func archiveTasks(ctx context.Context, tasks map[uuid.UUID]*dgraphStruct.DgraphTask) int {
	ids := make([]string, 0, len(tasks))
	for id := range tasks {
		ids = append(ids, id.String())
	}
	if err := archiveBusiness.ArchiveInGraphAndSearch(ctx, "tasks", ids); err != nil {
		helpers.LogWarnWithContext(ctx, "Import rollback could not take %d tasks off the graph and search: %+v", len(ids), err)
		return len(ids)
	}
	return 0
}

// oneByOne takes each node of a kind off the graph and search by archive, the
// kind's own delete.
func oneByOne[N any](entity string, archive func(context.Context, uuid.UUID, N) error) func(context.Context, map[uuid.UUID]N) int {
	return func(ctx context.Context, nodes map[uuid.UUID]N) int {
		left := 0
		for id, node := range nodes {
			if err := archive(ctx, id, node); err != nil {
				helpers.LogWarnWithContext(ctx, "Import rollback could not take %s %s off the graph and search: %+v", entity, id, err)
				left++
			}
		}
		return left
	}
}

// liveTasks reads the tasks of ids the graph has and hasn't deleted, in one
// read.
func liveTasks(ctx context.Context, ids []uuid.UUID, by *dgraphStruct.DgraphUser) (map[uuid.UUID]*dgraphStruct.DgraphTask, error) {
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = id.String()
	}
	tasks, err := taskDomain.GetDgraphTasksForWrite(ctx, keys, by.Uid)
	if err != nil {
		return nil, err
	}
	live := make(map[uuid.UUID]*dgraphStruct.DgraphTask, len(tasks))
	for _, t := range tasks {
		if id, err := uuid.Parse(t.Uuid); err == nil && !helpers.IsSoftDeleted(t.DeletedAt) {
			live[id] = t
		}
	}
	return live, nil
}

// liveProjects reads the projects of ids the graph has and hasn't deleted,
// one read each: an import makes few, and only an empty one is deleted. One
// the graph doesn't have (a run that never reached it) is none.
func liveProjects(ctx context.Context, ids []uuid.UUID, by *dgraphStruct.DgraphUser) (map[uuid.UUID]*dgraphStruct.DgraphProject, error) {
	live := map[uuid.UUID]*dgraphStruct.DgraphProject{}
	for _, id := range ids {
		p, err := projectBusiness.GetDgraphProjectInfoAndTeamAdminFlag(ctx, id.String(), by.Uid)
		if errors.Is(err, helpers.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if p != nil && p.Uuid != "" && !helpers.IsSoftDeleted(p.DeletedAt) {
			live[id] = p
		}
	}
	return live, nil
}

// liveTeams reads the teams of ids the graph has and hasn't deleted, one read
// each, as liveProjects does.
func liveTeams(ctx context.Context, ids []uuid.UUID, _ *dgraphStruct.DgraphUser) (map[uuid.UUID]*dgraphStruct.DgraphTeam, error) {
	live := map[uuid.UUID]*dgraphStruct.DgraphTeam{}
	for _, id := range ids {
		tm, err := teamBusiness.GetDgraphTeamInfoByUUIDForArchivingAndUnarchivingTeam(ctx, id.String())
		if errors.Is(err, helpers.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if tm != nil && tm.Uuid != "" && !helpers.IsSoftDeleted(tm.DeletedAt) {
			live[id] = tm
		}
	}
	return live, nil
}

// importRunner is whoever ran the import, in the graph: a rollback takes its
// things away as them.
func importRunner(ctx context.Context, job *importModels.Job) (*dgraphStruct.DgraphUser, error) {
	if job.TriggeredBy == nil {
		return nil, errors.New("the import names nobody who ran it")
	}
	by, err := userDomain.GetDgraphUserInfoByUUID(ctx, job.TriggeredBy.String())
	if err != nil || by == nil {
		return nil, fmt.Errorf("whoever ran it could not be read: %w", err)
	}
	return by, nil
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
	by, err := importRunner(ctx, job)
	if err != nil {
		return len(entries), err
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
