package business

// Asking for a task itself to be approved (package 9 R2b, basecamp-25's
// H-5): not a move a status rule holds, but "approve this", by approvers
// among the project's members (N of M) and, when asked, by its clients
// through the project's comment links. The same request and answers as a
// held move (one model); docs and files ask through the same Open.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// AskInput asks for an item itself to be approved.
type AskInput struct {
	Approvers []string   `json:"approvers"`
	Needed    int        `json:"needed"`
	Client    bool       `json:"client"`
	DueAt     *time.Time `json:"due_at"`
	Ask       string     `json:"ask"`
}

// MaxAsk bounds what is asked.
const MaxAsk = 500

// Refusals of an ask.
var (
	ErrWaitingAlready = errors.New("it's waiting for approval already")
	ErrNobodyToAsk    = errors.New("name who approves it, or ask the client")
	ErrNoClientLink   = errors.New("share a client link that can comment first, so the client can answer")
	ErrDuePassed      = errors.New("pick a due time that hasn't passed")
)

// Seams for tests: opening a request, and the clock.
var (
	openAppr = model.Open
	now      = time.Now
)

// AskForTask asks for a task itself to be approved, as who, who must be able
// to change the task.
func AskForTask(ctx context.Context, who *userModels.UserInfo, taskID uuid.UUID, in AskInput) (*View, error) {
	if helpers.ActsThroughToken(ctx) {
		return nil, ErrThroughToken
	}
	task, s, err := taskFor(ctx, taskID, who, workAccess.NeedEditTask)
	if err != nil {
		return nil, err
	}
	if task.Project == nil {
		return nil, fmt.Errorf("that task has no project")
	}
	projectID, err := uuid.Parse(task.Project.Uuid)
	if err != nil {
		return nil, fmt.Errorf("that task has no project")
	}
	in.Ask = strings.TrimSpace(in.Ask)
	if len([]rune(in.Ask)) > MaxAsk {
		return nil, fmt.Errorf("say what you ask in at most %d characters", MaxAsk)
	}
	if in.DueAt != nil && !in.DueAt.After(now()) {
		return nil, ErrDuePassed
	}
	approvers, err := people(ctx, projectID, in.Approvers, "approvers")
	if err != nil {
		return nil, err
	}
	approvers, cant, err := seeing(ctx, task, approvers)
	if err != nil {
		return nil, err
	}
	if len(cant) > 0 {
		return nil, fmt.Errorf("%s can't see this task, so can't approve it", and(cant))
	}
	if len(approvers) == 0 && !in.Client {
		return nil, ErrNobodyToAsk
	}
	needed := 0
	if len(approvers) > 0 {
		needed = min(max(in.Needed, 1), len(approvers))
	}
	var clients []string
	if in.Client {
		if clients, err = clientLinks(ctx, projectID, taskID); err != nil {
			return nil, err
		}
		if len(clients) == 0 {
			return nil, ErrNoClientLink
		}
	}
	a, created, err := openAppr(ctx, &model.Approval{
		ResourceKind: model.KindTask, ResourceID: taskID.String(), ProjectID: projectID, TaskID: taskID,
		Ask: in.Ask, DueAt: in.DueAt, RequestedBy: who.UserPostgresInfo.Id, Approvers: approvers, Needed: needed, ClientGrants: clients,
	})
	if err != nil {
		return nil, err
	}
	if !created {
		return nil, ErrWaitingAlready
	}
	line := "Asked for approval."
	if in.Ask != "" {
		line += " " + in.Ask
	}
	if _, err := comment(helpers.WithWorkflowGenerated(ctx), who, taskID, line); err != nil {
		helpers.LogErrorWithContext(ctx, "Approvals: ask on %s: %v", taskID, err)
	}
	if len(approvers) > 0 {
		afterRequest(ctx, a, task, &who.UserDgraphInfo, "")
	}
	return view(ctx, a, task, who, s), nil
}
