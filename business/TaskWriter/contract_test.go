package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// calls is what the default asked of today's code.
type calls struct {
	checked []workAccess.Need
	made    []string
	refuse  error
}

func (c *calls) use(t *testing.T) {
	t.Helper()
	oldFor, oldStatus, oldEstimate, oldDelete, oldRestore := taskFor, setStatus, setEstimate, deleteTask, restoreTask
	t.Cleanup(func() {
		taskFor, setStatus, setEstimate, deleteTask, restoreTask = oldFor, oldStatus, oldEstimate, oldDelete, oldRestore
	})
	taskFor = func(_ context.Context, _ uuid.UUID, _ *userModels.UserInfo, need workAccess.Need) (*dgraphStruct.DgraphTask, workAccess.Standing, error) {
		c.checked = append(c.checked, need)
		if c.refuse != nil {
			return nil, workAccess.Standing{}, c.refuse
		}
		return &dgraphStruct.DgraphTask{Uuid: "t"}, workAccess.Standing{}, nil
	}
	setStatus = func(_ context.Context, _ uuid.UUID, s string, _ *dgraphStruct.DgraphTask, _ *dgraphStruct.DgraphUser) error {
		c.made = append(c.made, "status "+s)
		return nil
	}
	setEstimate = func(_ context.Context, _ uuid.UUID, m int, _ *dgraphStruct.DgraphTask, _ *dgraphStruct.DgraphUser) error {
		if m < 0 {
			return taskBusiness.ErrEstimateRange
		}
		c.made = append(c.made, "estimate")
		return nil
	}
	deleteTask = func(context.Context, uuid.UUID, *dgraphStruct.DgraphTask, *dgraphStruct.DgraphUser) error {
		c.made = append(c.made, "delete")
		return nil
	}
	restoreTask = func(context.Context, uuid.UUID, *dgraphStruct.DgraphTask, *dgraphStruct.DgraphUser) error {
		c.made = append(c.made, "restore")
		return nil
	}
}

var who = &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1"}}

// Apply checks that the person may change the task (and, with a deletion in
// the batch, delete it), then makes each op through today's function, in
// order.
func TestApplyMakesTodaysChangesAfterTodaysCheck(t *testing.T) {
	c := &calls{}
	c.use(t)
	_, changes, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{
		{Kind: OpStatus, Value: json.RawMessage(`"done"`)}, {Kind: OpEstimate, Value: json.RawMessage(`90`)},
		{Kind: OpDelete}, {Kind: OpRestore},
	}, 0)
	if err != nil || len(changes) != 4 {
		t.Fatalf("%v %v", changes, err)
	}
	if len(c.checked) != 2 || c.checked[0] != workAccess.NeedDeleteTask || c.checked[1] != workAccess.NeedEditTask {
		t.Errorf("checked %v: for the deletion, then for the changes", c.checked)
	}
	if want := "status done,estimate,delete,restore"; strings.Join(c.made, ",") != want {
		t.Errorf("made %q, want %q", strings.Join(c.made, ","), want)
	}
	c.checked = nil
	if _, _, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{{Kind: OpStatus, Value: json.RawMessage(`"done"`)}}, 0); err != nil || len(c.checked) != 1 || c.checked[0] != workAccess.NeedEditTask {
		t.Errorf("a change alone: checked %v, %v", c.checked, err)
	}
}

// Someone who may not change the task changes nothing; an op the default
// doesn't make yet changes nothing either, the others in its batch included.
func TestApplyRefusesBeforeItChangesAnything(t *testing.T) {
	c := &calls{refuse: taskBusiness.ErrNotAllowed}
	c.use(t)
	if _, _, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{{Kind: OpDelete}}, 0); !errors.Is(err, taskBusiness.ErrNotAllowed) {
		t.Errorf("deleting someone else's task: %v", err)
	}
	c.refuse = nil
	if _, _, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{{Kind: OpStatus, Value: json.RawMessage(`"done"`)}, {Kind: OpName, Value: json.RawMessage(`"x"`)}}, 0); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a rename: %v", err)
	}
	if _, _, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{{Kind: OpStatus, Value: json.RawMessage(`7`)}}, 0); !errors.Is(err, ErrBadValue) {
		t.Errorf("a status that isn't one: %v", err)
	}
	if _, _, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{{Kind: OpEstimate, Value: json.RawMessage(`-5`)}}, 0); !errors.Is(err, taskBusiness.ErrEstimateRange) {
		t.Errorf("a negative estimate: %v", err)
	}
	if _, _, err := Tasks.Apply(context.Background(), nil, uuid.New(), []Op{{Kind: OpDelete}}, 0); !errors.Is(err, taskBusiness.ErrTaskNotVisible) {
		t.Errorf("nobody: %v", err)
	}
	if len(c.made) != 0 {
		t.Errorf("made %v", c.made)
	}
}
