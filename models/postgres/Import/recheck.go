package models

import (
	"context"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/akashc777/OneCamp/models/pgexec"
)

// MappedTask is a task an import made, as its id map has it: its kind
// (EntityTask or EntitySubtask), its source id, its id here, and what the
// import recorded of its source (project_source_id and author, the source id
// of whoever made it; a sub-task's record has no author).
type MappedTask struct {
	Entity        string
	SourceID      string
	TaskID        uuid.UUID
	ProjectSource string
	Author        string
}

const mappedTaskColumns = `m.entity_type, m.source_id, m.onecamp_uuid,
	COALESCE(m.metadata->>'project_source_id', ''), COALESCE(m.metadata->>'author', '')`

func scanMappedTask(r pgexec.Row) (MappedTask, error) {
	var t MappedTask
	err := r.Scan(&t.Entity, &t.SourceID, &t.TaskID, &t.ProjectSource, &t.Author)
	return t, err
}

// MappedTasksIn reads a page of the tasks an import made in a project, those
// in the trash too (a restore brings one back as it is): sub-tasks first,
// each kind by source id, from after (afterEntity, afterSource), the last
// page's last ("" for the first).
func MappedTasksIn(ctx context.Context, importID, projectID uuid.UUID, afterEntity, afterSource string, limit int) ([]MappedTask, error) {
	return pgexec.Query(ctx, `SELECT `+mappedTaskColumns+`
		FROM import_id_map m JOIN tasks t ON t.id = m.onecamp_uuid
		WHERE m.import_id = $1 AND m.entity_type IN ('subtask', 'task') AND m.created_by_this_import
		  AND t.project_id = $2 AND (m.entity_type, m.source_id) > ($3, $4)
		ORDER BY m.entity_type, m.source_id LIMIT $5`,
		scanMappedTask, importID, projectID, afterEntity, afterSource, limit)
}

// MappedTasksByID reads the tasks an import made with these ids here, those
// in the trash too; one the import no longer has is left out.
func MappedTasksByID(ctx context.Context, importID uuid.UUID, ids []uuid.UUID) ([]MappedTask, error) {
	in := make([]string, len(ids))
	for i, id := range ids {
		in[i] = id.String()
	}
	return pgexec.Query(ctx, `SELECT `+mappedTaskColumns+`
		FROM import_id_map m JOIN tasks t ON t.id = m.onecamp_uuid
		WHERE m.import_id = $1 AND m.entity_type IN ('subtask', 'task') AND m.created_by_this_import
		  AND m.onecamp_uuid = ANY($2::uuid[])
		ORDER BY m.entity_type, m.source_id`,
		scanMappedTask, importID, pq.Array(in))
}

// SampleMappedTasks reads up to n of the top-level tasks an import made, those
// in the trash too, by source id. A record that names no maker (Author "")
// proves only its project.
func SampleMappedTasks(ctx context.Context, importID uuid.UUID, n int) ([]MappedTask, error) {
	return pgexec.Query(ctx, `SELECT `+mappedTaskColumns+`
		FROM import_id_map m JOIN tasks t ON t.id = m.onecamp_uuid
		WHERE m.import_id = $1 AND m.entity_type = 'task' AND m.created_by_this_import
		ORDER BY m.source_id LIMIT $2`,
		scanMappedTask, importID, n)
}

// MappedComment is a comment an import made on a task: its source id and its
// id here.
type MappedComment struct {
	SourceID  string
	CommentID uuid.UUID
}

// MappedComments reads the live comments an import made on the tasks (their
// source ids), by task source id.
func MappedComments(ctx context.Context, importID uuid.UUID, taskSources []string) (map[string][]MappedComment, error) {
	type row struct {
		task string
		MappedComment
	}
	rows, err := pgexec.Query(ctx, `
		SELECT m.parent_source_id, m.source_id, m.onecamp_uuid
		FROM import_id_map m JOIN comments c ON c.id = m.onecamp_uuid AND c.deleted_at IS NULL
		WHERE m.import_id = $1 AND m.entity_type = 'comment' AND m.created_by_this_import
		  AND m.parent_source_id = ANY($2)
		ORDER BY m.parent_source_id, m.source_id`,
		func(r pgexec.Row) (row, error) {
			var c row
			err := r.Scan(&c.task, &c.SourceID, &c.CommentID)
			return c, err
		}, importID, pq.Array(taskSources))
	if err != nil {
		return nil, err
	}
	out := map[string][]MappedComment{}
	for _, c := range rows {
		out[c.task] = append(out[c.task], c.MappedComment)
	}
	return out, nil
}
