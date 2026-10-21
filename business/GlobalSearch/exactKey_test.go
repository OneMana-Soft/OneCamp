package business

import (
	"context"
	"testing"
	"time"

	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/google/uuid"
)

func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// A search for exactly a key finds its task first, only in the reader's own
// projects, never a deleted one, and the task shows once.
func TestAKeyTypedIntoSearchFindsItsTaskFirst(t *testing.T) {
	task, project := uuid.New(), uuid.NewString()
	live := time.Time{}
	gone := time.Now()
	deleted := false
	swap(t, &resolveKey, func(_ context.Context, raw string) (uuid.UUID, error) {
		if k, n, ok := taskKeys.Parse(raw); ok && k == "Q4" && n == 12 {
			return task, nil
		}
		return uuid.Nil, taskKeys.ErrNoTask
	})
	swap(t, &readTask, func(_ context.Context, id string, _ string) (*dgraphStruct.DgraphTask, error) {
		at := &live
		if deleted {
			at = &gone
		}
		return &dgraphStruct.DgraphTask{Uuid: id, Name: "Pricing page copy", Status: "inProgress", DeletedAt: at,
			Project:  &dgraphStruct.DgraphProject{Uuid: project, Name: "Q4 launch"},
			Assignee: &dgraphStruct.DgraphUser{Uuid: "ben", UserName: "Ben"}}, nil
	})
	mine := map[string]bool{project: true}

	hit := exactKeyHit(context.Background(), "q4-12", mine)
	if hit == nil || hit.Task == nil || hit.Task.Uuid != task.String() || hit.Task.TaskProjectName != "Q4 launch" {
		t.Fatalf("the key's hit: %+v", hit)
	}
	page := withExactKey(hit, []*openSearchStruct.GlobalSearchOpenSearchResp{
		{Type: openSearchStruct.DOC_TYPE},
		{Type: openSearchStruct.TASK_TYPE, Task: &openSearchStruct.OpenSearchTask{Uuid: task.String()}},
	})
	if len(page) != 2 || page[0] != hit || page[1].Type != openSearchStruct.DOC_TYPE {
		t.Errorf("the page: %+v; want the key's task first and once", page)
	}

	for what, got := range map[string]*openSearchStruct.GlobalSearchOpenSearchResp{
		"a project the reader isn't in": exactKeyHit(context.Background(), "Q4-12", map[string]bool{}),
		"a key no task has":             exactKeyHit(context.Background(), "Q4-13", mine),
		"words, not a key":              exactKeyHit(context.Background(), "pricing page", mine),
	} {
		if got != nil {
			t.Errorf("%s found %+v", what, got)
		}
	}
	deleted = true
	if got := exactKeyHit(context.Background(), "Q4-12", mine); got != nil {
		t.Errorf("a deleted task found: %+v", got)
	}
}

func TestEveryTaskHitCarriesItsKey(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	swap(t, &keysOf, func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
		if len(ids) != 2 {
			t.Errorf("read %d keys, want 2", len(ids))
		}
		return map[uuid.UUID]string{a: "Q4-1"}, nil
	})
	page := []*openSearchStruct.GlobalSearchOpenSearchResp{
		{Type: openSearchStruct.TASK_TYPE, Task: &openSearchStruct.OpenSearchTask{Uuid: a.String()}},
		{Type: openSearchStruct.TASK_TYPE, Task: &openSearchStruct.OpenSearchTask{Uuid: b.String()}},
		{Type: openSearchStruct.DOC_TYPE},
		nil,
	}
	keyTaskHits(context.Background(), page)
	if page[0].Task.TaskKey != "Q4-1" || page[1].Task.TaskKey != "" {
		t.Errorf("keys: %q %q", page[0].Task.TaskKey, page[1].Task.TaskKey)
	}
}
