package business

// Making a task from a template (Make): the project's facts read once, the
// template fitted over what was typed, its required properties asked for
// before anything is made, then the task and its sub-tasks made through the
// Task Writer as the maker.

import (
	"context"
	"fmt"
	"strings"

	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	projectTemplate "github.com/akashc777/OneCamp/business/ProjectTemplate"
	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// MissingError is a task its template asks more of: the properties, by
// their keys (taskWriter.Prop*), still to fill.
type MissingError struct{ Props []string }

func (e *MissingError) Error() string {
	return "this template asks for " + strings.Join(e.Props, ", ") + " before the task is made"
}

// Result is what making a task from a template made.
type Result struct {
	TaskID   uuid.UUID `json:"task_id"`
	Subtasks int       `json:"subtasks"`
	LeftOff  []string  `json:"left_off,omitempty"`
}

// Seams for tests.
var (
	projectFields func(context.Context, uuid.UUID) ([]*taskFieldModel.Field, error)             = taskFieldBusiness.List
	typesOf       func(context.Context, uuid.UUID) ([]taskKeyModel.Type, error)                 = workTaxonomy.Types
	statusesOf    func(context.Context, uuid.UUID) (*taskStatusBusiness.ProjectStatuses, error) = taskStatusBusiness.List
	assigneeFor                                                                                 = assignee
	makeTask                                                                                    = taskWriter.MakeTask
	makeSubTasks                                                                                = taskWriter.MakeSubTasks
)

// Make makes a task from t in a project as who, typed over, its days counted
// from start. Each preset the project can't take is left off and named in
// the result; a required property left empty refuses with a MissingError
// before anything is made.
func Make(ctx context.Context, who *userModels.UserInfo, project uuid.UUID, t Template, typed Typed, start projectTemplate.Start) (Result, error) {
	facts := Facts{Assignable: map[string]bool{}}
	var err error
	if facts.Fields, err = projectFields(ctx, project); err != nil {
		return Result{}, err
	}
	types, err := typesOf(ctx, project)
	if err != nil {
		return Result{}, err
	}
	for _, ty := range types {
		facts.Types = append(facts.Types, ty.ID)
	}
	statuses, err := statusesOf(ctx, project)
	if err != nil {
		return Result{}, err
	}
	if statuses != nil {
		for _, b := range statuses.BuiltIn {
			facts.Statuses = append(facts.Statuses, b.Key, b.Label)
		}
		for _, c := range statuses.Custom {
			facts.Statuses = append(facts.Statuses, c.Name)
		}
	}
	// Only the one assignee the task would get is looked up, as the graph
	// knows them, so the Task Writer gives it to them as the app does.
	var given *dgraphStruct.DgraphUser
	if a := pick(typed.Assignee, t.Shape.Assignee); a != "" {
		if given, err = assigneeFor(ctx, a, project); err != nil {
			return Result{}, err
		}
		facts.Assignable[a] = given != nil
	}
	made := Fit(t, typed, facts, start)
	if missing := taskWriter.Missing(t.Shape.Required, made.Task); len(missing) > 0 {
		return Result{LeftOff: made.LeftOff}, &MissingError{Props: missing}
	}
	if made.Task.Assignee != nil {
		made.Task.Assignee = given
	}
	id, err := makeTask(ctx, who, project, made.Task)
	if err != nil {
		return Result{LeftOff: made.LeftOff}, err
	}
	res := Result{TaskID: id, LeftOff: made.LeftOff}
	if len(made.Subtasks) == 0 {
		return res, nil
	}
	// Its sub-tasks are made together under it (TaskWriter MakeSubTasks). The
	// task is made, so a batch that fails names each sub-task left off, never
	// failing what a retry would make twice.
	parent := &dgraphStruct.DgraphTask{Uuid: id.String(), Project: &dgraphStruct.DgraphProject{Uuid: project.String()}}
	ids, err := makeSubTasks(ctx, who, parent, made.Subtasks)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/TaskTemplate Make sub-tasks err: %+v", err)
		for _, name := range made.Subtasks {
			res.LeftOff = append(res.LeftOff, fmt.Sprintf("The sub-task %q couldn't be made: add it to the task by hand.", name))
		}
		return res, nil
	}
	res.Subtasks = len(ids)
	return res, nil
}

// assignee is someone as the graph knows them while they may be given the
// project's tasks: an active account (Principal's CanBeAssigned, as forms
// ask) who is in the project or is an AI teammate (the Task Writer's
// Assignable); nil for anyone else.
func assignee(ctx context.Context, userUUID string, project uuid.UUID) (*dgraphStruct.DgraphUser, error) {
	ok, err := taskWriter.Assignable(ctx, userUUID, project.String())
	if err != nil || !ok {
		return nil, err
	}
	u, err := userBusiness.GetDgraphUserInfoByUUID(ctx, userUUID)
	if err != nil || u == nil || !principalBusiness.CanBeAssigned(u).Allowed {
		return nil, nil
	}
	return u, nil
}
