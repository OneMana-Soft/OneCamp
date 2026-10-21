package Domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// PR1: one builder for a task list's filters (the project's list and board,
// a person's list and board), where four controllers each wrote their own
// graph fragments.
func TestATaskListsFiltersAreBuiltInOnePlace(t *testing.T) {
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	f := TaskListFilter{
		Search: "launch",
		Params: []TaskListParam{
			{ID: "task_priority", Value: []any{"high", "low", `x") OR has(x`}},
			{ID: "task_status", Value: []any{"todo"}},
			{ID: "task_assignee_name", Value: []any{"0x1", "0x2"}},
			{ID: "task_project_name", Value: []any{"0x9"}},
			{ID: "overdue", Value: "1"},
			{ID: "task_name", Value: "copy"},
			{ID: "uid) OR has(x", Value: "y"},
			{ID: "task_cycle", Value: []any{"c1"}},
		},
		Now:    now,
		Status: func(v []string) string { return "STATUS(" + strings.Join(v, ",") + ")" },
		Side: func(id string, v []string) (string, bool) {
			return "SIDE(" + id + ")", id == "task_cycle"
		},
	}
	got, err := f.Clause()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`regexp(task_name, /.*launch.*/i)`,
		`anyofterms(task_priority,"high low")`,
		`STATUS(todo)`,
		`uid_in(task_assignee, [0x1, 0x2])`,
		`uid_in(task_project, [0x9])`,
		`lt(task_due_date, "2026-10-19T09:00:00Z")`,
		`regexp(task_name, /.*copy.*/i)`,
		`SIDE(task_cycle)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "has(x") {
		t.Fatalf("a disallowed column or value reached the query: %s", got)
	}
	if n := strings.Count(got, " AND "); n < 7 {
		t.Fatalf("clauses joined by AND: %s", got)
	}

	// A window: tasks due, starting or running inside it.
	from, to := now, now.AddDate(0, 0, 7)
	if got, _ := (TaskListFilter{From: &from, To: &to}).Clause(); !strings.HasPrefix(got, "((ge(task_due_date,") {
		t.Fatalf("window: %s", got)
	}
	// Due by: open tasks with a due date up to then (Home's "needs me": the
	// overdue and today's, not 60 days of everything).
	if got, _ := (TaskListFilter{DueBy: &to}).Clause(); !strings.Contains(got, `le(task_due_date, "2026-10-26T09:00:00Z")`) || !strings.Contains(got, `gt(task_due_date, "1970-01-01T00:00:00Z")`) || !strings.Contains(got, "not eq(task_status") {
		t.Fatalf("due by: %s", got)
	}
	// Nothing set: nothing.
	if got, err := (TaskListFilter{Search: "   "}).Clause(); got != "" || err != nil {
		t.Fatalf("empty: %q %v", got, err)
	}
	// A search that can't be made safe is the person's to fix.
	if _, err := (TaskListFilter{Search: strings.Repeat("a", 500)}).Clause(); !errors.Is(err, ErrSearchTerm) {
		t.Fatalf("a long search: %v", err)
	}
}
