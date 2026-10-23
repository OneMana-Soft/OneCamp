package helpers

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A context marks open only the tasks it names: anything said through it of
// another task still reads that task's restriction.
func TestAContextMarksOnlyItsOwnTasksOpen(t *testing.T) {
	made, other := uuid.New(), uuid.New()
	ctx := WithOpenTasks(context.Background(), made)
	for _, c := range []struct {
		name  string
		ctx   context.Context
		tasks []string
		want  bool
	}{
		{"a task it marks", ctx, []string{made.String()}, true},
		{"that task in capitals", ctx, []string{strings.ToUpper(made.String())}, true},
		{"that task and one it doesn't mark", ctx, []string{made.String(), other.String()}, false},
		{"no task", ctx, nil, false},
		{"an id that isn't a task's", ctx, []string{"t1"}, false},
		{"a context with no mark", context.Background(), []string{made.String()}, false},
	} {
		if got := TasksOpen(c.ctx, c.tasks...); got != c.want {
			t.Errorf("%s: open %v, want %v", c.name, got, c.want)
		}
	}
}
