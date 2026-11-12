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
	"github.com/akashc777/OneCamp/models/taskread"
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

// who has a user id, so a read made as them (taskread.ActingAs) is known
// by it.
var who = &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "5d3c8a52-8a0e-4c35-9b0f-2f1c1d7e6a01"}}

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

// A parent op puts the task under the task named, or with "" at the top,
// through the hierarchy's one change (business/Task Reparent), after the
// edit check; a value that isn't a task's id or "" changes nothing.
func TestApplyReparentsThroughTheHierarchy(t *testing.T) {
	c := &calls{}
	c.use(t)
	old := setParent
	t.Cleanup(func() { setParent = old })
	var got, readers []string
	setParent = func(ctx context.Context, _ uuid.UUID, parent string, _ *dgraphStruct.DgraphUser) error {
		got = append(got, "under "+parent)
		readers = append(readers, taskread.ReaderOf(ctx))
		return nil
	}
	p := uuid.New()
	for _, v := range []string{`"` + strings.ToUpper(p.String()) + `"`, `""`} {
		if _, _, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{{Kind: OpParent, Value: json.RawMessage(v)}}, 0); err != nil {
			t.Errorf("parent %s: %v", v, err)
		}
	}
	if want := "under " + p.String() + ",under "; strings.Join(got, ",") != want {
		t.Errorf("moved %q, want %q", strings.Join(got, ","), want)
	}
	// The new parent is read as who: one they can't see is not there.
	if len(readers) != 2 || readers[0] != who.UserDgraphInfo.Uuid || readers[1] != who.UserDgraphInfo.Uuid {
		t.Errorf("the hierarchy read as %q, want who (%s) each time", readers, who.UserDgraphInfo.Uuid)
	}
	if len(c.checked) != 2 || c.checked[0] != workAccess.NeedEditTask || c.checked[1] != workAccess.NeedEditTask {
		t.Errorf("checked %v, want the edit check each time", c.checked)
	}
	got = nil
	for _, v := range []string{`"x"`, `5`, `{}`} {
		if _, _, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{{Kind: OpParent, Value: json.RawMessage(v)}}, 0); !errors.Is(err, ErrBadValue) {
			t.Errorf("parent %s: %v, want ErrBadValue", v, err)
		}
	}
	if len(got) != 0 {
		t.Errorf("a bad parent moved %v", got)
	}
}

// A relation op links the task to another, or takes the link off, through
// the links' one change (business/Task ChangeLink), after the edit check;
// "blocks" is refused in words pointing to dependencies, and a value that
// doesn't name a task changes nothing.
func TestApplyLinksThroughTheOneChange(t *testing.T) {
	c := &calls{}
	c.use(t)
	old := setRelation
	t.Cleanup(func() { setRelation = old })
	var got []taskBusiness.LinkAsk
	var readers []string
	setRelation = func(ctx context.Context, _ uuid.UUID, ask taskBusiness.LinkAsk, _ *dgraphStruct.DgraphUser) error {
		got = append(got, ask)
		readers = append(readers, taskread.ReaderOf(ctx))
		return nil
	}
	other := uuid.New()
	v := `{"kind":"duplicates","task":"` + strings.ToUpper(other.String()) + `","direction":"in","remove":true}`
	if _, _, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{{Kind: OpRelation, Value: json.RawMessage(v)}}, 0); err != nil {
		t.Fatalf("relation: %v", err)
	}
	if want := (taskBusiness.LinkAsk{Kind: "duplicates", Other: other.String(), Direction: "in", Remove: true}); len(got) != 1 || got[0] != want {
		t.Errorf("linked %+v, want %+v", got, want)
	}
	// The other task is read as who: one they can't see is not there.
	if len(readers) != 1 || readers[0] != who.UserDgraphInfo.Uuid {
		t.Errorf("the link read as %q, want who (%s)", readers, who.UserDgraphInfo.Uuid)
	}
	if len(c.checked) != 1 || c.checked[0] != workAccess.NeedEditTask {
		t.Errorf("checked %v, want the edit check", c.checked)
	}
	got = nil
	blocks := `{"kind":"blocks","task":"` + other.String() + `"}`
	if _, _, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{{Kind: OpRelation, Value: json.RawMessage(blocks)}}, 0); !errors.Is(err, taskBusiness.ErrLink) || !strings.Contains(err.Error(), "Dependencies") {
		t.Errorf("blocks: %v, want a refusal pointing to dependencies", err)
	}
	for _, v := range []string{`{"kind":"relates","task":"x"}`, `"relates"`, `{"kind":"relates"}`} {
		if _, _, err := Tasks.Apply(context.Background(), who, uuid.New(), []Op{{Kind: OpRelation, Value: json.RawMessage(v)}}, 0); !errors.Is(err, ErrBadValue) {
			t.Errorf("relation %s: %v, want ErrBadValue", v, err)
		}
	}
	if len(got) != 0 {
		t.Errorf("a refused relation linked %+v", got)
	}
}
