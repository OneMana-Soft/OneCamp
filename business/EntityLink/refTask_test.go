package entityLinkBusiness

import (
	"context"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// EVLINK head A: a task is a thing a link may name (RefTask, as a calendar
// event's link names one), and whether someone may open it is the task
// view's rule, read as its chip is (readTasks): the one rule, never a
// second. A member of its project opens it; someone outside it, a deleted
// task and one that isn't there are refused.
func TestCanReadRefOpensATaskByTheTaskViewsRule(t *testing.T) {
	const (
		member  = "c0000000-0000-4000-8000-000000000001"
		outside = "c0000000-0000-4000-8000-000000000002"
		deleted = "c0000000-0000-4000-8000-000000000003"
		missing = "c0000000-0000-4000-8000-000000000004"
		proj    = "d0000000-0000-4000-8000-000000000001"
	)
	saved := readTaskChips
	t.Cleanup(func() { readTaskChips = saved })
	gone := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	reads := 0
	readTaskChips = func(_ context.Context, ids []string, readerUID string) ([]*dgraphStruct.DgraphTask, error) {
		reads++
		if readerUID != reader.UserUID {
			t.Fatalf("read for %q", readerUID)
		}
		return []*dgraphStruct.DgraphTask{
			{Uuid: member, Name: "Press kit", Project: project(proj, 1, 0)},
			{Uuid: outside, Name: "Salaries", Project: project(proj, 0, 0)},
			{Uuid: deleted, Name: "Old", Project: project(proj, 1, 1), DeletedAt: &gone},
		}, nil
	}
	for id, want := range map[string]bool{member: true, outside: false, deleted: false, missing: false} {
		got, err := CanReadRef(context.Background(), "task", id, reader.UserUID, reader.UserUUID)
		if err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", id, got, err, want)
		}
	}
	if reads != 4 {
		t.Errorf("the task view's rule was read %d times for 4 asks", reads)
	}
}
