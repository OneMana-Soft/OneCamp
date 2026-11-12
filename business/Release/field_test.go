package business

import (
	"context"
	"testing"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/google/uuid"
)

// The release filter (jira-04): the tasks in the chosen releases, and with
// "No release" every task but those in some release and none of the chosen.
// Nothing chosen that could be a release matches nothing, unread.
func TestTheReleaseFilterClause(t *testing.T) {
	was := tasksFiltered
	t.Cleanup(func() { tasksFiltered = was })
	var reads int
	var none bool
	var answer []string
	tasksFiltered = func(_ context.Context, _ uuid.UUID, _ []uuid.UUID, n bool) ([]string, error) {
		reads++
		none = n
		return answer, nil
	}
	ctx, project, r := context.Background(), uuid.New(), uuid.NewString()
	for _, c := range []struct {
		values []string
		answer []string
		want   string
		none   bool
		reads  int
	}{
		{[]string{"junk"}, nil, taskDomain.NoTask, false, 0},
		{[]string{r}, []string{"a", "b"}, `eq(task_uuid, ["a","b"])`, false, 1},
		{[]string{r}, nil, taskDomain.NoTask, false, 1},
		{[]string{NoRelease, r}, []string{"a"}, `NOT eq(task_uuid, ["a"])`, true, 1},
		{[]string{NoRelease}, nil, "", true, 1},
	} {
		reads, none, answer = 0, false, c.answer
		got, err := FilterClause(ctx, project, c.values)
		if err != nil || got != c.want || none != c.none || reads != c.reads {
			t.Errorf("%v over %v: %q (none %v, %d reads, %v), want %q", c.values, c.answer, got, none, reads, err, c.want)
		}
	}
	// The web sends these.
	if FilterID != "task_release" || NoRelease != "none" {
		t.Errorf("the filter's id and its none: %s %s", FilterID, NoRelease)
	}
}
