package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// The ops rules make: priority, assignee and due, through the same check.
func (c *calls) useRuleOps(t *testing.T, members ...string) {
	t.Helper()
	oldP, oldA, oldD, oldM, oldU, oldS := setPriority, setAssignee, setDates, isMember, personByUUID, unseeing
	t.Cleanup(func() {
		setPriority, setAssignee, setDates, isMember, personByUUID, unseeing = oldP, oldA, oldD, oldM, oldU, oldS
	})
	// The project's people see its unrestricted tasks.
	unseeing = func(context.Context, string, *dgraphStruct.DgraphTask, *dgraphStruct.DgraphUser) error { return nil }
	setPriority = func(_ context.Context, _ uuid.UUID, p string, _ *dgraphStruct.DgraphTask, _ *dgraphStruct.DgraphUser) error {
		c.made = append(c.made, "priority "+p)
		return nil
	}
	setAssignee = func(_ context.Context, _ uuid.UUID, to *dgraphStruct.DgraphUser, _, _ string, _ *dgraphStruct.DgraphTask, _ *dgraphStruct.DgraphUser) error {
		if to == nil {
			c.made = append(c.made, "unassign")
		} else {
			c.made = append(c.made, "assign "+to.Uuid)
		}
		return nil
	}
	setDates = func(_ context.Context, _ uuid.UUID, d taskBusiness.TaskDates, _ *dgraphStruct.DgraphTask, _ *dgraphStruct.DgraphUser) error {
		if d.Due == nil {
			t.Fatal("a due op set no due date")
		}
		if d.Due.IsZero() {
			c.made = append(c.made, "due cleared")
		} else {
			c.made = append(c.made, "due "+d.Due.UTC().Format("2006-01-02"))
		}
		return nil
	}
	isMember = func(_ context.Context, user, _ string) (bool, error) {
		for _, m := range members {
			if m == user {
				return true, nil
			}
		}
		return false, nil
	}
	personByUUID = func(_ context.Context, id string) (*dgraphStruct.DgraphUser, error) {
		return &dgraphStruct.DgraphUser{Uid: "0x9", Uuid: id}, nil
	}
}

func TestApplyMakesPriorityAssigneeAndDue(t *testing.T) {
	maya := uuid.NewString()
	c := &calls{}
	c.use(t)
	c.useRuleOps(t, maya)
	_, changes, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{
		{Kind: OpPriority, Value: json.RawMessage(`"high"`)},
		{Kind: OpAssignee, Value: json.RawMessage(`"` + maya + `"`)},
		{Kind: OpAssignee, Value: json.RawMessage(`""`)},
		{Kind: OpDue, Value: json.RawMessage(`"2026-11-02T00:00:00Z"`)},
		{Kind: OpDue, Value: json.RawMessage(`null`)},
	}, 0)
	if err != nil || len(changes) != 5 {
		t.Fatalf("%v %v", changes, err)
	}
	want := "priority high,assign " + maya + ",unassign,due 2026-11-02,due cleared"
	if got := strings.Join(c.made, ","); got != want {
		t.Errorf("made %q, want %q", got, want)
	}
}

// A priority that isn't one, someone outside the project, and a date that
// doesn't read change nothing.
func TestApplyRefusesValuesTheRulesWouldBreak(t *testing.T) {
	c := &calls{}
	c.use(t)
	c.useRuleOps(t)
	for _, op := range []Op{
		{Kind: OpPriority, Value: json.RawMessage(`"someday"`)},
		{Kind: OpDue, Value: json.RawMessage(`"next tuesday"`)},
		{Kind: OpAssignee, Value: json.RawMessage(`"not-a-uuid"`)},
	} {
		if _, _, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{op}, 0); !errors.Is(err, ErrBadValue) {
			t.Errorf("%s %s: %v", op.Kind, op.Value, err)
		}
	}
	if _, _, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{{Kind: OpAssignee, Value: json.RawMessage(`"` + uuid.NewString() + `"`)}}, 0); !errors.Is(err, ErrNotInProject) {
		t.Errorf("assigning someone outside the project: %v", err)
	}
	if len(c.made) != 0 {
		t.Errorf("made %v", c.made)
	}
}
