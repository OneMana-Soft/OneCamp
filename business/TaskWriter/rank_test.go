package business

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// L-1: a task's place in its project's one order is the Task Writer's rank
// op: checked as any change to the task, then placed between the tasks named.
func TestARankOpPlacesTheTask(t *testing.T) {
	c := &calls{}
	c.use(t)
	old := reorder
	t.Cleanup(func() { reorder = old })
	var got struct{ project, before, after, task string }
	reorder = func(_ context.Context, project string, moved []*dgraphStruct.DgraphTask, before, after string) error {
		got.project, got.before, got.after, got.task = project, before, after, moved[0].Uuid
		return nil
	}
	taskID := uuid.New()
	value, _ := json.Marshal(map[string]string{"before": "a", "after": "b"})
	if _, _, err := Tasks.Apply(context.Background(), who, taskID, []Op{{Kind: OpRank, Value: value}}, 0); err != nil {
		t.Fatal(err)
	}
	if len(c.checked) != 1 || got.task != taskID.String() || got.before != "a" || got.after != "b" {
		t.Fatalf("checked %v, placed %+v", c.checked, got)
	}
	if _, _, err := Tasks.Apply(context.Background(), who, taskID, []Op{{Kind: OpRank, Value: json.RawMessage(`"x"`)}}, 0); !errors.Is(err, ErrBadValue) {
		t.Fatalf("a place that isn't one: %v", err)
	}
}
