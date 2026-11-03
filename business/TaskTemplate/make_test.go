package business

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	projectTemplate "github.com/akashc777/OneCamp/business/ProjectTemplate"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

type makeCalls struct {
	task     *taskWriter.NewTask
	subtasks []string
	parents  []string
	// batches counts the sub-task batches.
	batches int
}

// project stands in for a project with a Size field, the built-in statuses,
// sam (who may be given its tasks, graph uid 0x5) and nobody else; making
// sub-tasks that name fail fails them all, as one batch fails.
func project(t *testing.T, made uuid.UUID, size *taskFieldModel.Field, sam string, fail string) *makeCalls {
	saved := struct {
		f   func(context.Context, uuid.UUID) ([]*taskFieldModel.Field, error)
		ty  func(context.Context, uuid.UUID) ([]taskKeyModel.Type, error)
		st  func(context.Context, uuid.UUID) (*taskStatusBusiness.ProjectStatuses, error)
		a   func(context.Context, string, uuid.UUID) (*dgraphStruct.DgraphUser, error)
		mt  func(context.Context, *userModels.UserInfo, uuid.UUID, taskWriter.NewTask) (uuid.UUID, error)
		mss func(context.Context, *userModels.UserInfo, *dgraphStruct.DgraphTask, []string) ([]uuid.UUID, error)
	}{projectFields, typesOf, statusesOf, assigneeFor, makeTask, makeSubTasks}
	t.Cleanup(func() {
		projectFields, typesOf, statusesOf, assigneeFor, makeTask, makeSubTasks = saved.f, saved.ty, saved.st, saved.a, saved.mt, saved.mss
	})
	calls := &makeCalls{}
	projectFields = func(context.Context, uuid.UUID) ([]*taskFieldModel.Field, error) {
		return []*taskFieldModel.Field{size}, nil
	}
	typesOf = func(context.Context, uuid.UUID) ([]taskKeyModel.Type, error) { return nil, nil }
	statusesOf = func(context.Context, uuid.UUID) (*taskStatusBusiness.ProjectStatuses, error) {
		return &taskStatusBusiness.ProjectStatuses{BuiltIn: []taskStatusBusiness.BuiltIn{{Key: "todo", Label: "To do"}, {Key: "done", Label: "Done"}}}, nil
	}
	assigneeFor = func(_ context.Context, who string, _ uuid.UUID) (*dgraphStruct.DgraphUser, error) {
		if who == sam {
			return &dgraphStruct.DgraphUser{Uid: "0x5", Uuid: sam}, nil
		}
		return nil, nil
	}
	makeTask = func(_ context.Context, _ *userModels.UserInfo, _ uuid.UUID, in taskWriter.NewTask) (uuid.UUID, error) {
		calls.task = &in
		return made, nil
	}
	makeSubTasks = func(_ context.Context, _ *userModels.UserInfo, parent *dgraphStruct.DgraphTask, names []string) ([]uuid.UUID, error) {
		calls.batches++
		calls.parents = append(calls.parents, parent.Uuid)
		if slices.Contains(names, fail) {
			return nil, errors.New("graph down")
		}
		calls.subtasks = append(calls.subtasks, names...)
		ids := make([]uuid.UUID, len(names))
		for i := range ids {
			ids[i] = uuid.New()
		}
		return ids, nil
	}
	return calls
}

var today = projectTemplate.Start{Day: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Loc: time.UTC}

// A task is made from a template as its maker, with its presets (the
// assignee as the graph knows them) and then its sub-tasks under it, in order.
func TestATaskIsMadeFromATemplateAndItsSubtasksUnderIt(t *testing.T) {
	made, sam := uuid.New(), uuid.NewString()
	size := &taskFieldModel.Field{ID: uuid.New(), Name: "Size", Type: taskFieldModel.TypeText}
	calls := project(t, made, size, sam, "")
	tpl := kept(t, Template{Name: "Bug report", Shape: Shape{
		Task: projectTemplate.Task{Name: "[Bug]", Status: "To do", Subtasks: []projectTemplate.Subtask{{Name: "Reproduce"}, {Name: "Fix"}}},
		Type: workTaxonomy.TypeBug.String(), Assignee: sam, Fields: map[string]json.RawMessage{size.ID.String(): json.RawMessage(`"L"`)},
	}})
	res, err := Make(context.Background(), &userModels.UserInfo{}, uuid.New(), tpl, Typed{Name: str("Checkout crashes")}, today)
	if err != nil {
		t.Fatal(err)
	}
	if res.TaskID != made || res.Subtasks != 2 || len(res.LeftOff) != 0 {
		t.Errorf("made %+v", res)
	}
	task := calls.task
	if task == nil || task.Name != "Checkout crashes" || task.Status != "To do" || task.Assignee == nil || task.Assignee.Uid != "0x5" ||
		task.Type == nil || *task.Type != workTaxonomy.TypeBug || string(task.Fields[size.ID]) != `"L"` {
		t.Fatalf("the task was made as %+v", task)
	}
	if calls.batches != 1 || !slices.Equal(calls.subtasks, []string{"Reproduce", "Fix"}) || len(calls.parents) != 1 || calls.parents[0] != made.String() {
		t.Errorf("sub-tasks %v under %v in %d batches; want one", calls.subtasks, calls.parents, calls.batches)
	}
}

// A template's required properties are asked for before anything is made.
func TestARequiredPropertyLeftEmptyRefusesBeforeAnythingIsMade(t *testing.T) {
	size := &taskFieldModel.Field{ID: uuid.New(), Name: "Size", Type: taskFieldModel.TypeText}
	calls := project(t, uuid.New(), size, uuid.NewString(), "")
	need := []string{taskWriter.PropDue, taskWriter.PropField + size.ID.String(), taskWriter.PropAssignee}
	tpl := kept(t, Template{Name: "Bug report", Shape: Shape{Task: projectTemplate.Task{Name: "[Bug]"}, Assignee: uuid.NewString(), Required: need}})
	_, err := Make(context.Background(), &userModels.UserInfo{}, uuid.New(), tpl, Typed{}, today)
	var missing *MissingError
	if !errors.As(err, &missing) || !slices.Equal(missing.Props, need) {
		t.Fatalf("Make = %v, want a MissingError for %v (the preset assignee can't be given the task)", err, need)
	}
	if calls.task != nil || len(calls.parents) != 0 {
		t.Errorf("something was made: %+v, %v", calls.task, calls.parents)
	}
	due := time.Now()
	if _, err := Make(context.Background(), &userModels.UserInfo{}, uuid.New(), tpl, Typed{
		Due: &due, Fields: map[string]json.RawMessage{size.ID.String(): json.RawMessage(`"M"`)}, Assignee: str(""),
	}, today); !errors.As(err, &missing) || !slices.Equal(missing.Props, []string{taskWriter.PropAssignee}) {
		t.Errorf("with the due date and the size typed, Make = %v, want only the assignee missing", err)
	}
}

// A preset the project can't take, and sub-tasks that couldn't be made, are
// named; the task is still made.
func TestWhatCouldntBeMadeIsNamedAndTheTaskStillMade(t *testing.T) {
	made := uuid.New()
	size := &taskFieldModel.Field{ID: uuid.New(), Name: "Size", Type: taskFieldModel.TypeText}
	calls := project(t, made, size, uuid.NewString(), "Fix")
	tpl := kept(t, Template{Name: "Bug report", Shape: Shape{
		Task:     projectTemplate.Task{Name: "[Bug]", Subtasks: []projectTemplate.Subtask{{Name: "Reproduce"}, {Name: "Fix"}, {Name: "Verify"}}},
		Assignee: uuid.NewString(),
	}})
	res, err := Make(context.Background(), &userModels.UserInfo{}, uuid.New(), tpl, Typed{}, today)
	if err != nil {
		t.Fatal(err)
	}
	words := strings.Join(res.LeftOff, " | ")
	// The sub-tasks are made together, so a batch that fails leaves each of
	// them off, by name.
	if res.TaskID != made || res.Subtasks != 0 || !strings.Contains(words, "assignee") ||
		!strings.Contains(words, `"Reproduce"`) || !strings.Contains(words, `"Fix"`) || !strings.Contains(words, `"Verify"`) {
		t.Errorf("made %+v", res)
	}
	if calls.task == nil || calls.task.Assignee != nil || calls.batches != 1 || len(calls.subtasks) != 0 {
		t.Errorf("made %+v with sub-tasks %v in %d batches", calls.task, calls.subtasks, calls.batches)
	}
}

// Choosing Task is choosing a type (CR-167 F2): a template that asks for one
// is met by it, and the task is made as a Task.
func TestChoosingTaskMeetsATemplatesRequiredType(t *testing.T) {
	size := &taskFieldModel.Field{ID: uuid.New(), Name: "Size", Type: taskFieldModel.TypeText}
	calls := project(t, uuid.New(), size, uuid.NewString(), "")
	tpl := kept(t, Template{Name: "Bug report", Shape: Shape{Task: projectTemplate.Task{Name: "[Bug]"}, Required: []string{taskWriter.PropType}}})
	task := workTaxonomy.TypeTask.String()
	if _, err := Make(context.Background(), &userModels.UserInfo{}, uuid.New(), tpl, Typed{Type: &task}, today); err != nil {
		t.Fatalf("Make with Task chosen = %v, want the task made", err)
	}
	if calls.task == nil || calls.task.Type == nil || *calls.task.Type != workTaxonomy.TypeTask {
		t.Errorf("the task was made as %+v, want a Task", calls.task)
	}
}
