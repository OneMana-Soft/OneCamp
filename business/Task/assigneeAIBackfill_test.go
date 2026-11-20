package business

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Task"
)

// AI only, like assigneeAIBackfill.go and assigneeAI.go.

// fakeTasks is a graph of n tasks, task-i given to user-i (every third to
// nobody), read a page at a time as domain.TaskAssigneePage reads it.
func fakeTasks(n int) func(context.Context, int, int) ([]domain.TaskAssignee, error) {
	return func(_ context.Context, offset, first int) ([]domain.TaskAssignee, error) {
		var page []domain.TaskAssignee
		for i := offset; i < n && i < offset+first; i++ {
			to := fmt.Sprintf("user-%d", i)
			if i%3 == 0 {
				to = ""
			}
			page = append(page, domain.TaskAssignee{Task: fmt.Sprintf("task-%d", i), Assignee: to})
		}
		return page, nil
	}
}

func seamAIAssignees(t *testing.T) {
	t.Helper()
	wasPage, wasNow, wasSet, wasOn := assigneePage, assigneesNow, setAIAssignees, aiOn
	wasReindex, wasGo, wasWait := aiReindexing, goRetry, aiAssigneeRetryWait
	t.Cleanup(func() {
		assigneePage, assigneesNow, setAIAssignees, aiOn = wasPage, wasNow, wasSet, wasOn
		aiReindexing, goRetry, aiAssigneeRetryWait = wasReindex, wasGo, wasWait
	})
	aiReindexing = func() bool { return false }
	aiOn = func() bool { return true }
}

// The repair writes every task's assignee now, a page in one write, each
// task's own (mutants: no write; a map keyed wrong), and counts what the
// writes corrected.
func TestTheAIAssigneeRepairWritesEachPageInOneWrite(t *testing.T) {
	seamAIAssignees(t)
	assigneePage = fakeTasks(2*aiAssigneeBackfillPage + 50)
	var calls []map[string]string
	setAIAssignees = func(_ context.Context, to map[string]string) (int, error) {
		calls = append(calls, to)
		return 1, nil
	}
	rep, err := backfillAIAssignees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("%d writes for three pages, want 3", len(calls))
	}
	for i, call := range calls {
		want := map[string]string{}
		page, _ := fakeTasks(2*aiAssigneeBackfillPage+50)(context.Background(), i*aiAssigneeBackfillPage, aiAssigneeBackfillPage)
		for _, p := range page {
			want[p.Task] = p.Assignee
		}
		if !reflect.DeepEqual(call, want) {
			t.Errorf("page %d wrote %d tasks' assignees, not each task's own (%d)", i, len(call), len(want))
		}
	}
	if rep.Tasks != 2*aiAssigneeBackfillPage+50 || rep.Corrected != 3 {
		t.Errorf("report %+v, want %d tasks and 3 corrected", rep, 2*aiAssigneeBackfillPage+50)
	}
}

// A write that fails fails the repair, so it isn't recorded done (mutant: the
// key set though a write failed), and a rebuild of the index refuses it.
func TestTheAIAssigneeRepairIsNotDoneWhileAWriteFails(t *testing.T) {
	seamAIAssignees(t)
	assigneePage = fakeTasks(2*aiAssigneeBackfillPage + 1)
	writes := 0
	setAIAssignees = func(_ context.Context, to map[string]string) (int, error) {
		writes++
		if writes == 2 {
			return 0, errors.New("opensearch down")
		}
		return len(to), nil
	}
	if _, err := backfillAIAssignees(context.Background()); err == nil {
		t.Fatal("a failed write left the repair answering done")
	}
	if writes != 2 {
		t.Errorf("%d writes after the failed one's, want it to stop there", writes-2)
	}

	writes = 0
	aiReindexing = func() bool { return true }
	if _, err := backfillAIAssignees(context.Background()); err == nil || writes != 0 {
		t.Errorf("during a rebuild of the index the repair wrote %d times (err %v)", writes, err)
	}
	if AIAssigneeBackfill.Key != "ai_embeddings_task_assignee_v1" {
		t.Errorf("the repair's key is %q", AIAssigneeBackfill.Key)
	}
}

// A hand-over whose AI write failed is written once more, in the background,
// with the tasks' assignees as they are by then; and only once.
func TestAFailedAIHandOverWriteIsTriedOnceMoreWithTheAssigneeNow(t *testing.T) {
	seamAIAssignees(t)
	aiAssigneeRetryWait = time.Millisecond
	goRetry = func(_ string, f func()) { f() }
	// Handed over to u1, then to u9 before the retry.
	assigneesNow = func(_ context.Context, ids []string) (map[string]string, error) {
		if len(ids) != 1 || ids[0] != "t1" {
			t.Errorf("the retry read %v, want t1", ids)
		}
		return map[string]string{"t1": "u9"}, nil
	}
	var calls []map[string]string
	setAIAssignees = func(_ context.Context, to map[string]string) (int, error) {
		calls = append(calls, to)
		return 0, errors.New("opensearch down")
	}
	if err := aiAssigneeWrite(context.Background(), map[string]string{"t1": "u1"}); err == nil {
		t.Error("a failed write answered no error")
	}
	if len(calls) != 2 {
		t.Fatalf("%d writes, want the hand-over's and one retry", len(calls))
	}
	if !reflect.DeepEqual(calls[1], map[string]string{"t1": "u9"}) {
		t.Errorf("the retry wrote %v, not the task's assignee now", calls[1])
	}
}

// With AI off the repair isn't started: it reads no page and records nothing
// (where AI was never on there is no index for it to write to, so every try
// would fail). With AI on it runs and records (mutant: the aiOn check dropped).
func TestTheAIAssigneeRepairStartsOnlyWithAIOn(t *testing.T) {
	seamAIAssignees(t)
	wasStart := startAIAssigneeOnce
	t.Cleanup(func() { startAIAssigneeOnce = wasStart })
	pages := 0
	assigneePage = func(ctx context.Context, offset, first int) ([]domain.TaskAssignee, error) {
		pages++
		return fakeTasks(3)(ctx, offset, first)
	}
	setAIAssignees = func(context.Context, map[string]string) (int, error) { return 0, nil }
	// The repair as Once.Run does it, at once and with the key kept here: the
	// pass, then its key once the pass has succeeded.
	var recorded []string
	startAIAssigneeOnce = func(ctx context.Context) {
		if _, err := AIAssigneeBackfill.Repair(ctx); err == nil {
			recorded = append(recorded, AIAssigneeBackfill.Key)
		}
	}
	aiOn = func() bool { return false }
	StartAIAssigneeBackfill(context.Background())
	if pages != 0 || len(recorded) != 0 {
		t.Fatalf("with AI off the repair read %d pages and recorded %v, want nothing", pages, recorded)
	}
	aiOn = func() bool { return true }
	StartAIAssigneeBackfill(context.Background())
	if pages != 1 || !reflect.DeepEqual(recorded, []string{aiAssigneeBackfillKey}) {
		t.Fatalf("with AI on the repair read %d pages and recorded %v, want 1 page and its key", pages, recorded)
	}
}
