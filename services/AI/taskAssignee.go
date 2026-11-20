package ai

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A task's entry in the AI index names its assignee, and a search grants the
// task to them (buildPermissionFilter), in its project or not, as the app
// does. The entry was written once, when the task was made, so whoever a task
// was first given to kept reading it through Ask AI after it went to someone
// else, or after they left its project and were taken off it.
//
// So a hand-over is written onto the entry (SetTaskAssignees), and every
// search keeps a task it reached by that grant only for the task's assignee
// now, read from the task itself (keepAssigned): an entry left naming someone
// the task has gone from (written before hand-overs were written, or by a
// write that failed) gives them nothing.

// taskAssigneeScript writes params.to[the entry's task] as its assignee (""
// takes it off), and leaves an entry already right, or of a task not in
// params, as it is.
const taskAssigneeScript = `def to = params.to[ctx._source.content_uuid];
if (to == null) {
	ctx.op = 'noop'
} else if (to == '') {
	if (ctx._source.containsKey('task_assignee_user_id')) { ctx._source.remove('task_assignee_user_id') } else { ctx.op = 'noop' }
} else if (to.equals(ctx._source.task_assignee_user_id)) {
	ctx.op = 'noop'
} else {
	ctx._source.task_assignee_user_id = to
}`

// updateTaskEntries is updateEntries; a seam for tests that count the writes.
var updateTaskEntries = updateEntries

// taskAssigneeBatch is the most tasks one hand-over write asks for.
const taskAssigneeBatch = 500

// SetTaskAssignees writes hand-overs (task uuid -> its assignee now, a user
// uuid or "" for nobody) onto the tasks' AI entries, in one update-by-query
// for every taskAssigneeBatch tasks, and answers how many entries said
// something else. One hand-over is one update-by-query; a bulk run's page is
// one too. The comments on a task copy no assignee, so they are left as they
// are; a task never changes project after it is made, so its project is left
// too.
func SetTaskAssignees(ctx context.Context, to map[string]string) (corrected int, err error) {
	ids := make([]string, 0, len(to))
	clean := make(map[string]string, len(to))
	for task, assignee := range to {
		if task = strings.TrimSpace(task); task != "" {
			ids = append(ids, task)
			clean[task] = strings.TrimSpace(assignee)
		}
	}
	sort.Strings(ids)
	for len(ids) > 0 {
		page := ids[:min(len(ids), taskAssigneeBatch)]
		ids = ids[len(page):]
		params := make(map[string]any, len(page))
		for _, id := range page {
			params[id] = clean[id]
		}
		body, err := json.Marshal(map[string]any{
			"script": map[string]any{
				"source": taskAssigneeScript,
				"lang":   "painless",
				"params": map[string]any{"to": params},
			},
			"query": map[string]any{"bool": map[string]any{"filter": []any{
				map[string]any{"term": map[string]any{"content_type": "task"}},
				map[string]any{"terms": map[string]any{"content_uuid": page}},
			}}},
		})
		if err != nil {
			return corrected, err
		}
		n, err := updateTaskEntries(ctx, body, "tasks' assignees")
		corrected += n
		if err != nil {
			return corrected, err
		}
	}
	return corrected, nil
}

// taskAssigneesNow reads tasks' assignees now. A seam for tests.
var taskAssigneesNow = taskDomain.TaskAssignees

// taskAssigneeAsItIs is the task's assignee now (a user uuid, "" for nobody),
// in one read of the graph, for its entry about to be written. had, what the
// write was asked with, stays when the read fails or the graph doesn't hold
// the task: a miss at worst, never a leak (keepAssigned).
func taskAssigneeAsItIs(ctx context.Context, task, had string) string {
	now, err := taskAssigneesNow(ctx, []string{task})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AI: could not read task %s's assignee before writing its entry, so it is written as asked: %v", task, err)
		return had
	}
	for id, to := range now {
		if strings.EqualFold(id, task) {
			return to
		}
	}
	return had
}

// taskReader is someone a search reads for, with the projects whose tasks it
// grants them: the person asking, and the person an agent's run is for
// (runPermissionFilter), whose grants must both hold.
type taskReader struct {
	user     string
	projects map[string]bool
}

func readerIn(user string, projects []string) taskReader {
	r := taskReader{user: strings.TrimSpace(user), projects: make(map[string]bool, len(projects))}
	for _, p := range projects {
		r.projects[strings.ToLower(strings.TrimSpace(p))] = true
	}
	return r
}

// covers is whether the search grants the reader a task in project by the
// project alone. A task in no project is never covered.
func (r taskReader) covers(project string) bool {
	project = strings.ToLower(strings.TrimSpace(project))
	return project != "" && r.projects[project]
}

// tasksAsTheyAre reads the named tasks' assignee, maker and project, in one
// read of the graph. A seam for tests.
var tasksAsTheyAre = func(ctx context.Context, taskUUIDs []string, reader string) ([]*dgraphStruct.DgraphTask, error) {
	return taskDomain.GetTasksStandingFor(ctx, taskUUIDs, reader)
}

// keepAssigned leaves out of what a search found each task, and each comment
// on a task, that a reader reached by being its assignee, or the maker of a
// task in no project, and no longer is: what isn't in one of the reader's
// projects is checked against the task as it is now (domain/Task
// TaskReader.MayRead, with no project to grant it), in one read for every
// item and reader, made only when some item needs it. A read that fails keeps
// none of those items: the search fails closed, and comes back short.
func keepAssigned[T any](ctx context.Context, readers []taskReader, items []T, of func(T) (task, project string, ok bool)) []T {
	var ask []string
	asked := map[string]bool{}
	for _, it := range items {
		task, project, ok := of(it)
		if !ok || asked[task] {
			continue
		}
		for _, r := range readers {
			if !r.covers(project) {
				asked[task] = true
				ask = append(ask, task)
				break
			}
		}
	}
	if len(ask) == 0 {
		return items
	}
	// ask is empty with no reader, so readers[0] is there.
	now := make(map[string]*dgraphStruct.DgraphTask, len(ask))
	tasks, err := tasksAsTheyAre(ctx, ask, readers[0].user)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AI: could not read who %d tasks a search found are given to now, so they are left out: %v", len(ask), err)
	}
	for _, t := range tasks {
		if t != nil {
			now[strings.ToLower(t.Uuid)] = t
		}
	}
	out := make([]T, 0, len(items))
	for _, it := range items {
		task, project, ok := of(it)
		if ok && asked[task] && !assignedOrMade(now[task], readers, project) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// assignedOrMade is whether every reader whose projects don't cover the
// entry's project may read the task as it is by being its assignee, or the
// maker of a task in no project.
func assignedOrMade(t *dgraphStruct.DgraphTask, readers []taskReader, project string) bool {
	for _, r := range readers {
		if r.covers(project) {
			continue
		}
		if t == nil || r.user == "" {
			return false
		}
		inProject := t.Project != nil && strings.TrimSpace(t.Project.Uuid) != ""
		if !(taskDomain.TaskReader{Assignee: isUser(t.Assignee, r.user), Creator: isUser(t.CreatedBy, r.user)}).MayRead(inProject) {
			return false
		}
	}
	return true
}

func isUser(u *dgraphStruct.DgraphUser, user string) bool {
	return u != nil && u.Uuid != "" && strings.EqualFold(u.Uuid, user)
}

// similarTask is the task a search result is, or is a comment on.
func similarTask(r SimilarResult) (task, project string, ok bool) {
	return taskOfEntry(r.ContentType, r.ContentUUID, r.TaskUUID, r.ProjectUUID)
}

// scopedTask is similarTask for FetchUnreadAcrossScopes.
func scopedTask(c ScopedContent) (task, project string, ok bool) {
	return taskOfEntry(c.ContentType, c.ContentUUID, c.TaskUUID, c.ProjectUUID)
}

func taskOfEntry(kind, content, task, project string) (string, string, bool) {
	switch kind {
	case "task":
		if task == "" {
			task = content
		}
	case "comment":
	default:
		return "", "", false
	}
	task = strings.ToLower(strings.TrimSpace(task))
	return task, project, task != ""
}
