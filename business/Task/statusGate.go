package business

import (
	"context"
	"errors"
	"fmt"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// StatusGate is asked before any task changes status (UpdateTaskStatusByTaskUUID:
// the app, the API, boards, GitHub, agents and rules): a project's status
// rules (who may move a task there, what it must have) and its approvals (a
// move that needs approving becomes a request). It answers nil to let the
// move happen, or an error saying why not. business/Approvals sets it from
// its init, so this package imports none of it; nil lets every move through.
var StatusGate func(ctx context.Context, task *dgraphStruct.DgraphTask, prev, next taskStatusBusiness.Resolved, by *dgraphStruct.DgraphUser) error

// WentOn is told when a task goes on without its open request for approval:
// it changed status by any path, which ends a held move (an approved move's
// request is closed before the move, so that one finds none open), or it was
// deleted, which ends an ask about the task itself too. business/Approvals
// sets it from its init to close the request, so a late answer can't pull the
// task back; nil leaves requests as they are.
var WentOn func(ctx context.Context, taskID uuid.UUID, deleted bool)

// StatusRefusal is a status change the gate didn't make, in words a person
// reads ("Only the project's admins move tasks to Done"). ApprovalID is set
// when the move became a request for approval instead: it happens once the
// approvers say yes.
type StatusRefusal struct {
	Why        string
	ApprovalID string
}

func (r *StatusRefusal) Error() string { return r.Why }

// RefusedStatus is the refusal err carries, if it is one.
func RefusedStatus(err error) (*StatusRefusal, bool) {
	var r *StatusRefusal
	ok := errors.As(err, &r)
	return r, ok
}

// ErrWantsApproval is the gate's answer about a new task's first status
// (FirstStatus) when a move there wants approving: the task is made in its
// project's default status, and the move is asked for once it is (askFirst).
var ErrWantsApproval = errors.New("tasks: moving it to that status wants approval")

// FirstStatuses is the gate's word on a batch of new tasks' first statuses
// (CreateTasks), asked once for the lot: each draft's status in next, as a
// move into it from its project's default. It answers the first refusal in
// the batch's order, else ErrWantsApproval when any of them wants approving,
// else nil. business/Approvals sets it from its init, with one read of the
// project's rules for every status the batch asks for; nil lets every first
// status through.
var FirstStatuses func(ctx context.Context, drafts []*dgraphStruct.DgraphTask, next []taskStatusBusiness.Resolved, by *dgraphStruct.DgraphUser) error

// firstStatusesOf asks FirstStatuses about the drafts whose first status
// isn't their project's default (def), which is never asked, as firstStatus
// asks about one; a data move's statuses aren't asked at all.
func firstStatusesOf(ctx context.Context, drafts []*dgraphStruct.DgraphTask, def taskStatusBusiness.Resolved, next []taskStatusBusiness.Resolved, by *dgraphStruct.DgraphUser) error {
	if FirstStatuses == nil || ctx.Value(dataMoveKey{}) != nil {
		return nil
	}
	var asked []*dgraphStruct.DgraphTask
	var to []taskStatusBusiness.Resolved
	for i, d := range drafts {
		if next[i] != def {
			asked, to = append(asked, d), append(to, next[i])
		}
	}
	if len(asked) == 0 {
		return nil
	}
	return FirstStatuses(ctx, asked, to, by)
}

type firstStatusKey struct{}

// FirstStatus says the gate is asked about a new task's first status, as a
// move into it from the project's default: a dry run, as there's no task yet
// to ask for. The gate answers who may and what it must have as for any move,
// and ErrWantsApproval where a move would become a request.
func FirstStatus(ctx context.Context) bool {
	return ctx.Value(firstStatusKey{}) != nil
}

type dataMoveKey struct{}

// DataMove marks tasks made as a move of data that has its statuses and its
// people already (a template's apply, an import and its re-check, GitHub's
// import and sync, the demo's seed), by someone with the right to make it: a
// first status there isn't a person's move, so the gate isn't asked, and an
// assignee it brings isn't a person's hand-over, so Unseeing isn't either.
func DataMove(ctx context.Context) context.Context {
	return context.WithValue(ctx, dataMoveKey{}, true)
}

// Held is a task made in its project's default status because the status it
// was asked for wants approval, and what became of asking: the request
// (ApprovalID, empty when none could be opened) and both in words ("Made in To
// do. Sent for approval: it moves to In review once it's approved.").
type Held struct {
	Status     string
	ApprovalID string
	Why        string
}

// firstStatus asks the gate about a new task's first status (next) as a move
// from the project's default (def), which is never asked itself: nil to make
// it in next, ErrWantsApproval to make it in def and then ask, or the gate's
// refusal, and nothing is made. draft is the task as it would be made.
func firstStatus(ctx context.Context, draft *dgraphStruct.DgraphTask, def, next taskStatusBusiness.Resolved, by *dgraphStruct.DgraphUser) error {
	if StatusGate == nil || next == def || ctx.Value(dataMoveKey{}) != nil {
		return nil
	}
	return StatusGate(context.WithValue(ctx, firstStatusKey{}, true), draft, def, next, by)
}

// readFor is a project as read for a new task's maker, with its id, for the
// task the rules read: the maker's standing there comes from that read.
func readFor(p *dgraphStruct.DgraphProject, id uuid.UUID) *dgraphStruct.DgraphProject {
	read := *p
	read.Uuid = id.String()
	return &read
}

// askFirst asks for the move to next that a task just made in its project's
// default status wanted approving: the request opens, or the gate says why
// none could, and the task stays. nil when the move was made after all (its
// rule changed since).
func askFirst(ctx context.Context, made *dgraphStruct.DgraphTask, next taskStatusBusiness.Resolved, by *dgraphStruct.DgraphUser) *Held {
	def := taskStatusBusiness.Of(made)
	id, _ := uuid.Parse(made.Uuid)
	err := UpdateTaskStatusByTaskUUID(ctx, id, next.Value(), made, by)
	if err == nil {
		return nil
	}
	held := &Held{Status: def.Value()}
	if r, ok := RefusedStatus(err); ok {
		held.ApprovalID, held.Why = r.ApprovalID, fmt.Sprintf("Made in %s. %s", def.Display(), r.Why)
		return held
	}
	helpers.LogErrorWithContext(ctx, "business/Task: asking to move the new task %s to %s: %v", made.Uuid, next.Value(), err)
	held.Why = fmt.Sprintf("Made in %s. Couldn't ask for approval to move it to %s. Try again in a moment.", def.Display(), next.Display())
	return held
}
