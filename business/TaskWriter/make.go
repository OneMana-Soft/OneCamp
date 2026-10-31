package business

// Making tasks, subtasks and comments for sources without a request: rules
// today, forms and intake after them. Each checks what the person may do in
// the project or on the task (Work Access's one check, as the handlers do),
// then makes it through business/Task's own function, so it is logged,
// searched, notified and told to listeners exactly as one made in the app.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The longest name a task made here takes (the app's own limit).
const maxTaskName = 200

// MaxCommentLength bounds a comment a source makes.
const MaxCommentLength = 4000

// NewTask is a task to make.
type NewTask struct {
	Name        string
	Description string
	Priority    string
	// Status is its first status, which meets the project's status rules as
	// the app's create does (CreateTask): a rule on who or on required fields
	// refuses in words, and nothing is made. One that wants approval makes it
	// in the default and asks, and nobody is told so here, so every caller
	// passes the default (forms To do, a rule's create task none).
	Status string
	Label  string
	// Due is when it is due; nil for no due date.
	Due *time.Time
	// Assignee gets the task when they are in the project; someone who isn't
	// (any more) leaves it unassigned rather than failing whoever sent it.
	Assignee *dgraphStruct.DgraphUser
	// Fields are values of the project's task fields, each as the field keeps
	// it (TaskField Normalize), set as the task is made.
	Fields map[uuid.UUID]json.RawMessage
	// Attachments are files the maker uploaded to the project, which go on
	// the task as it is made; any other is left out (CreateTask's OwnFiles).
	Attachments []*dgraphStruct.DgraphAttachment
}

// Seams for tests.
var (
	projectFor = func(ctx context.Context, projectID string, who *userModels.UserInfo) (*dgraphStruct.DgraphProject, error) {
		return projectBusiness.GetBasicDgraphProjectInfo(ctx, projectID, who.UserDgraphInfo.Uid)
	}
	projectWithTask = func(ctx context.Context, projectID, taskID string, who *userModels.UserInfo) (*dgraphStruct.DgraphProject, error) {
		return projectBusiness.GetBasicDgraphProjectInfoWithTaskUUID(ctx, projectID, who.UserDgraphInfo.Uid, taskID)
	}
	standingIn = func(ctx context.Context, who *userModels.UserInfo, p *dgraphStruct.DgraphProject) (workAccess.Standing, error) {
		return workAccess.Of(ctx, who.UserDgraphInfo.Uuid, p)
	}
	createTask    = taskBusiness.CreateTask
	createSubTask = taskBusiness.CreateSubTask
	createComment = taskBusiness.CreateTaskComment
	inProject     = projectBusiness.HasMember
	isAgent       = userDomain.IsAgentPrincipal
	setNewFields  = taskFieldModel.SetNew
)

// Assignable says whether someone may be given a project's task: one of its
// people, or an AI teammate (an agent's principal), given work as it is
// assigned in the app.
func Assignable(ctx context.Context, userUUID, projectID string) (bool, error) {
	ok, err := inProject(ctx, userUUID, projectID)
	if err != nil || ok {
		return ok, err
	}
	id, err := uuid.Parse(userUUID)
	return err == nil && isAgent(ctx, id), nil
}

// mayIn checks who may do need in the project.
func mayIn(ctx context.Context, who *userModels.UserInfo, p *dgraphStruct.DgraphProject, need workAccess.Need) error {
	if p == nil || p.Uuid == "" {
		return taskBusiness.ErrTaskNotVisible
	}
	s, err := standingIn(ctx, who, p)
	if err != nil {
		return err
	}
	if !s.Can(workAccess.NeedView) {
		return taskBusiness.ErrTaskNotVisible
	}
	return s.Check(need)
}

// cleanName is a task's name as it is made: one line, at most maxTaskName
// bytes on a character boundary.
func cleanName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxTaskName {
		return s
	}
	cut := maxTaskName
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// MakeTask makes a task in a project as who.
func MakeTask(ctx context.Context, who *userModels.UserInfo, projectID uuid.UUID, in NewTask) (uuid.UUID, error) {
	if who == nil {
		return uuid.Nil, taskBusiness.ErrTaskNotVisible
	}
	name := cleanName(in.Name)
	if name == "" {
		return uuid.Nil, fmt.Errorf("%w: a task needs a name", ErrBadValue)
	}
	p, err := projectFor(ctx, projectID.String(), who)
	if err != nil {
		return uuid.Nil, err
	}
	if err := mayIn(ctx, who, p, workAccess.NeedCreateTask); err != nil {
		return uuid.Nil, err
	}
	input := adapter.CreateOrUpdateTaskInput{
		TaskName: name, TaskDescription: in.Description, Priority: in.Priority,
		Status: in.Status, Label: in.Label, ProjectUuid: projectID.String(), Attachments: in.Attachments,
	}
	if in.Due != nil {
		input.DueDate = in.Due.Format(time.RFC3339)
	}
	assignee := in.Assignee
	if assignee != nil {
		ok, err := Assignable(ctx, assignee.Uuid, projectID.String())
		if err != nil {
			return uuid.Nil, err
		}
		if !ok {
			assignee = nil
		} else {
			input.AssigneeUuid = assignee.Uuid
		}
	}
	taskID, err := createTask(ctx, projectID, who, p, assignee, input, nil)
	if err != nil || len(in.Fields) == 0 {
		return taskID, err
	}
	// The task is made; a field value that didn't stick is logged, not turned
	// into a failure that would make the task twice on a retry.
	if err := setNewFields(ctx, taskID, in.Fields, who.UserPostgresInfo.Id); err != nil {
		helpers.LogErrorWithContext(ctx, "business/TaskWriter MakeTask fields err: %+v", err)
	}
	return taskID, nil
}

// MakeSubTask makes a subtask under a task as who.
func MakeSubTask(ctx context.Context, who *userModels.UserInfo, parent *dgraphStruct.DgraphTask, name string) (*dgraphStruct.DgraphTask, error) {
	if who == nil || parent == nil || parent.Project == nil {
		return nil, taskBusiness.ErrTaskNotVisible
	}
	name = cleanName(name)
	if name == "" {
		return nil, fmt.Errorf("%w: a subtask needs a name", ErrBadValue)
	}
	p, err := projectWithTask(ctx, parent.Project.Uuid, parent.Uuid, who)
	if err != nil {
		return nil, err
	}
	if p == nil || len(p.Tasks) == 0 {
		return nil, taskBusiness.ErrTaskNotVisible
	}
	if err := mayIn(ctx, who, p, workAccess.NeedCreateTask); err != nil {
		return nil, err
	}
	projectID, err := uuid.Parse(p.Uuid)
	if err != nil {
		return nil, err
	}
	return createSubTask(ctx, projectID, who, p, nil, adapter.CreateOrUpdateTaskInput{
		Uuid: parent.Uuid, ProjectUuid: p.Uuid, TaskName: name,
	})
}

// ErrEmptyComment is a comment with nothing in it.
var ErrEmptyComment = errors.New("tasks: a comment needs words")

// Comment comments on a task as who, and answers the comment's id.
func Comment(ctx context.Context, who *userModels.UserInfo, taskID uuid.UUID, body string) (string, error) {
	return CommentAs(ctx, who, who, taskID, body)
}

// CommentAs comments on a task with who's right to (Work Access, as Comment),
// written as by: a bot that speaks for the work, such as the workspace's
// assistant answering a rule's question. by never widens what who may do.
func CommentAs(ctx context.Context, who, by *userModels.UserInfo, taskID uuid.UUID, body string) (string, error) {
	if by == nil {
		by = who
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "", ErrEmptyComment
	}
	if len(body) > MaxCommentLength {
		return "", fmt.Errorf("%w: a comment is at most %d characters", ErrBadValue, MaxCommentLength)
	}
	task, _, err := taskFor(ctx, taskID, who, workAccess.NeedComment)
	if err != nil {
		return "", err
	}
	out, err := createComment(ctx, taskID, task, by, &adapter.CreateOrUpdateTaskCommentInput{
		TaskUuid: taskID.String(), CommentBody: body,
	}, nil)
	if err != nil {
		return "", err
	}
	if out == nil {
		return "", nil
	}
	return out.Uuid, nil
}
