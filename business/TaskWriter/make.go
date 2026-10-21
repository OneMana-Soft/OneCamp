package business

// Making tasks, sub-tasks and comments for sources without a request: rules
// today, forms and intake after them. Each checks what the person may do in
// the project or on the task (Work Access's one check, as the handlers do),
// then makes it through business/Task's own function, so it is logged,
// searched, notified and told to listeners exactly as one made in the app.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
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
	Status      string
	Label       string
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
)

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
	return createTask(ctx, projectID, who, p, nil, adapter.CreateOrUpdateTaskInput{
		TaskName: name, TaskDescription: in.Description, Priority: in.Priority,
		Status: in.Status, Label: in.Label, ProjectUuid: projectID.String(),
	}, nil)
}

// MakeSubTask makes a sub-task under a task as who.
func MakeSubTask(ctx context.Context, who *userModels.UserInfo, parent *dgraphStruct.DgraphTask, name string) (*dgraphStruct.DgraphTask, error) {
	if who == nil || parent == nil || parent.Project == nil {
		return nil, taskBusiness.ErrTaskNotVisible
	}
	name = cleanName(name)
	if name == "" {
		return nil, fmt.Errorf("%w: a sub-task needs a name", ErrBadValue)
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
	out, err := createComment(ctx, taskID, task, who, &adapter.CreateOrUpdateTaskCommentInput{
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
