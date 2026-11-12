package business

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// UX-PROJECTS-H1-BE (PROJECTS-14): No priority is a priority to set, so the
// op takes it off (a rule's undo puts back a task's No priority). It was
// refused as a bad value.
func TestApplyTakesThePriorityOffWithNone(t *testing.T) {
	c := &calls{}
	c.use(t)
	c.useRuleOps(t)
	_, changes, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{
		{Kind: OpPriority, Value: json.RawMessage(`""`)},
	}, 0)
	if err != nil || len(changes) != 1 {
		t.Fatalf("%v %v", changes, err)
	}
	if got := strings.Join(c.made, ","); got != "priority " {
		t.Errorf("made %q, want the priority taken off", got)
	}
}
