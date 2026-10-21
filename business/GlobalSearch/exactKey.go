package business

import (
	"context"

	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/google/uuid"
)

// Seams for tests.
var (
	resolveKey = taskKeys.Resolve
	readTask   = taskDomain.GetDgraphBasicTaskInfoByUUID
	keysOf     = taskKeys.KeysOf
)

// noReader is a uid that matches nobody: the task's read here needs no
// reader's counts, since the reader's own projects decide.
const noReader = "0x0"

// exactKeyHit is the task a search for exactly its key names ("Q4-12" or
// "q4-12"), to show first (the search engine's exact-match step, docs C2).
// It is found only in the reader's own projects, the same rule the index's
// task hits keep, so a key of a task they can't open finds nothing more than
// its words would. Anything else, or a failed read, is nil.
func exactKeyHit(ctx context.Context, text string, readerProjects map[string]bool) *openSearchStruct.GlobalSearchOpenSearchResp {
	if _, _, ok := taskKeys.Parse(text); !ok {
		return nil
	}
	id, err := resolveKey(ctx, text)
	if err != nil {
		return nil
	}
	t, err := readTask(ctx, id.String(), noReader)
	if err != nil || t == nil || t.Project == nil || !readerProjects[t.Project.Uuid] {
		return nil
	}
	if t.DeletedAt != nil && !t.DeletedAt.IsZero() && t.DeletedAt.Year() > 1970 {
		return nil
	}
	// Its current key comes with every task hit's (keyTaskHits), so an old
	// key typed finds the task under the key it has now.
	hit := &openSearchStruct.OpenSearchTask{
		Uuid: t.Uuid, TaskName: t.Name, TaskStatus: t.Status, TaskPriority: t.Priority,
		TaskProjectUuid: t.Project.Uuid, TaskProjectName: t.Project.Name,
	}
	if t.Assignee != nil {
		a := t.Assignee.Uuid
		hit.TaskAssigneeUuid = &a
		hit.TaskAssigneeFullName = t.Assignee.DisplayName()
	}
	return &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.TASK_TYPE, Task: hit}
}

// withExactKey puts the key's task first in a page and leaves its own hit
// further down out, so it shows once.
func withExactKey(first *openSearchStruct.GlobalSearchOpenSearchResp, page []*openSearchStruct.GlobalSearchOpenSearchResp) []*openSearchStruct.GlobalSearchOpenSearchResp {
	if first == nil {
		return page
	}
	out := make([]*openSearchStruct.GlobalSearchOpenSearchResp, 0, len(page)+1)
	out = append(out, first)
	for _, h := range page {
		if h != nil && h.Task != nil && h.Task.Uuid == first.Task.Uuid {
			continue
		}
		out = append(out, h)
	}
	return out
}

// keyTaskHits sets each task hit's key, in one read. A read that failed
// leaves them without keys: the page still goes.
func keyTaskHits(ctx context.Context, page []*openSearchStruct.GlobalSearchOpenSearchResp) {
	var ids []uuid.UUID
	for _, h := range page {
		if h != nil && h.Task != nil && h.Task.TaskKey == "" {
			if id, err := uuid.Parse(h.Task.Uuid); err == nil {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	keys, err := keysOf(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GlobalSearch couldn't read %d task hits' keys: %+v", len(ids), err)
		return
	}
	for _, h := range page {
		if h != nil && h.Task != nil && h.Task.TaskKey == "" {
			if id, err := uuid.Parse(h.Task.Uuid); err == nil {
				h.Task.TaskKey = keys[id]
			}
		}
	}
}
