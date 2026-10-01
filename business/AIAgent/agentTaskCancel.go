package business

// Stopping an AI teammate's work.
//
// A durable job could be started but never stopped: once handed to an agent it
// ran to a terminal state (or sat blocked) whether or not the person who asked
// still wanted it. There was no way to halt a misdirected run short of waiting
// for a wall-clock limit or restarting a worker — unacceptable for an agent that
// writes to real workspace data, and the one control every comparable coding
// agent surfaces first.
//
// Cancellation is COOPERATIVE and lease-respecting (see migration 134):
//   - a request is recorded on the job (who, when);
//   - a job that is not running has nothing in flight, so it is settled at once;
//   - a RUNNING job is stopped by its own worker: the lease heartbeat learns of
//     the request in the round trip that already proves ownership, cancels the
//     run context, and the worker writes the single terminal row — so there is
//     never a second writer racing the owner, and the agent still gets to say
//     honestly what it had already done.
//
// Authorization deliberately does NOT require workspace admin: the people who
// live with the work — whoever asked for it, the user it runs as, and the agent's
// owner — can stop it. An id alone is never enough.

import (
	"context"
	"errors"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// ErrAgentWorkNotFound / ErrAgentWorkForbidden let the controller answer with
// the right status without leaking whether a job exists.
var (
	ErrAgentWorkNotFound  = errors.New("agent work not found")
	ErrAgentWorkForbidden = errors.New("not allowed to stop this agent work")
)

// CancelResult tells the caller what the stop actually did, so the UI can say
// something true instead of a generic "ok".
type CancelResult struct {
	// Outcome is model.CancelStopped (already terminal), CancelRequested (the
	// worker is wrapping up), or CancelNoop (nothing open to stop).
	Outcome string `json:"outcome"`
	// State is the job's coarse user-facing state after the call.
	State string `json:"state"`
	// Message is a short, human sentence for a toast.
	Message string `json:"message"`
}

// CancelAgentWork stops an open durable job on the actor's behalf.
//
// Idempotent: stopping an already-stopping or already-finished job is not an
// error, it just reports what is true — a person clicking "Stop" twice must not
// see a failure.
func CancelAgentWork(ctx context.Context, actor Actor, taskID uuid.UUID) (CancelResult, error) {
	t, err := model.GetAgentTaskByID(ctx, taskID)
	if err != nil {
		return CancelResult{}, err
	}
	if t == nil {
		return CancelResult{}, ErrAgentWorkNotFound
	}
	agent, err := model.GetAgentByID(ctx, t.AgentId)
	if err != nil {
		return CancelResult{}, err
	}
	if agent == nil {
		// The agent is gone; only an admin can tidy up its leftovers.
		if !actor.IsAdmin {
			return CancelResult{}, ErrAgentWorkForbidden
		}
	} else if !canStopAgentWork(actor, taskPrincipals(agent.CreatedBy, t)) {
		return CancelResult{}, ErrAgentWorkForbidden
	}

	outcome, err := model.RequestAgentTaskCancel(ctx, taskID, actor.UserID)
	if err != nil {
		return CancelResult{}, err
	}
	// Tell every watcher immediately: "Stopping…" (or gone, for a job that had not
	// started) must appear on the surface at once, not on the next timer tick —
	// pressing Stop and seeing nothing change is what makes people press it again.
	if outcome != model.CancelNoop {
		publishAgentWorkChanged(ctx, taskID)
	}
	res := CancelResult{Outcome: outcome}
	switch outcome {
	case model.CancelStopped:
		res.State = string(ActiveWorkStopped)
		res.Message = "Stopped."
	case model.CancelRequested:
		res.State = string(ActiveWorkStopping)
		res.Message = "Stopping — it'll wrap up and post what it managed to do."
	default:
		res.State = string(activeWorkState(t.State))
		res.Message = "This work has already finished."
	}
	helpers.LogInfoWithContext(ctx, "agentWork: stop requested (task=%s by=%s outcome=%s)", taskID, actor.UserID, outcome)
	return res, nil
}

// workPrincipals are the people attached to one job: who owns the agent, who
// asked for the work, and whose permissions it runs with. Extracted as a tiny
// value so the ONE authorization rule below can be applied to any shape a job
// arrives in (the full task row, or a joined active-work row) — a second copy of
// this rule for a different struct is how permissions quietly diverge.
type workPrincipals struct {
	AgentOwner  uuid.UUID
	TriggeredBy *uuid.UUID
	RunAs       *uuid.UUID
}

// taskPrincipals reads the principals off a full job row.
func taskPrincipals(agentOwner uuid.UUID, t *model.AgentTask) workPrincipals {
	if t == nil {
		return workPrincipals{}
	}
	return workPrincipals{AgentOwner: agentOwner, TriggeredBy: t.TriggeredBy, RunAs: t.RunAsUserId}
}

// activeTaskPrincipals reads the principals off a joined active-work row.
func activeTaskPrincipals(t *model.AgentActiveTask) workPrincipals {
	if t == nil {
		return workPrincipals{}
	}
	return workPrincipals{AgentOwner: t.AgentCreatedBy, TriggeredBy: t.TriggeredBy, RunAs: t.RunAsUserId}
}

// canStopAgentWork reports whether the actor may stop this job. Pure, so the
// policy is unit-testable: a workspace admin, the agent's owner, the person who
// asked for the work, or the user it runs as. Nobody else — a job can post into
// a channel/DM the actor can't see, so "can see it" is not the bar for stopping
// it.
func canStopAgentWork(actor Actor, p workPrincipals) bool {
	if actor.IsAdmin {
		return true
	}
	if actor.UserID == uuid.Nil || p.AgentOwner == uuid.Nil && p.TriggeredBy == nil && p.RunAs == nil {
		return false
	}
	switch {
	case p.AgentOwner != uuid.Nil && actor.UserID == p.AgentOwner:
		return true
	case p.TriggeredBy != nil && *p.TriggeredBy == actor.UserID:
		return true
	case p.RunAs != nil && *p.RunAs == actor.UserID:
		return true
	}
	return false
}

// -- worker side --------------------------------------------------------------

// stoppedByHumanNote is the message the agent posts when a person stops it. It
// deliberately does not apologise or imply a failure: the run did exactly what
// was asked of it.
const stoppedByHumanNote = "Stopped — I've left things as they are."

// stoppedNote names WHO stopped the work when that can be resolved. In a shared
// thread, "stopped" with no author leaves everyone else guessing whether a person
// or a fault ended it; naming the requester is the same courtesy a human teammate
// would extend, and it costs one lookup on a path that runs once per stop.
// Falls back to the anonymous wording rather than dropping the message.
func stoppedNote(ctx context.Context, requestedBy *uuid.UUID) string {
	if requestedBy == nil || *requestedBy == uuid.Nil {
		return stoppedByHumanNote
	}
	name := steeringAuthorName(context.WithoutCancel(ctx), *requestedBy)
	if name == "" {
		return stoppedByHumanNote
	}
	return "Stopped at " + name + "'s request — I've left things as they are."
}

// agentWorkStopRequested reports whether a stop has been requested for this job.
// The claimed row's own flag is authoritative when set (it was read at claim
// time); otherwise it asks the store on a detached context, because the run
// context is exactly the thing that gets cancelled when a stop lands.
func agentWorkStopRequested(ctx context.Context, t *model.AgentTask) bool {
	if t == nil {
		return false
	}
	if t.CancelRequestedAt != nil {
		return true
	}
	requested, err := model.AgentTaskCancelRequested(context.WithoutCancel(ctx), t.Id)
	if err != nil {
		// A DB blip must never be read as "stop": that would end healthy runs.
		return false
	}
	return requested
}

// settleStoppedAgentWork closes a job a human stopped: it posts one honest
// message (including any partial work the run had produced), notifies the person
// who asked for the work, and records the terminal 'cancelled' row.
//
// Returns false when no stop was requested, so a caller can use it as a guard
// in front of its normal outcome handling:
//
//	if settleStoppedAgentWork(...) { return }
//
// Lease-guarded through commitAgentTaskState, so a worker that already lost the
// lease writes nothing.
func settleStoppedAgentWork(ctx context.Context, t *model.AgentTask, lease uuid.UUID, partial string, runID *uuid.UUID, postStatus, notify func(string)) bool {
	if !agentWorkStopRequested(ctx, t) {
		return false
	}
	note := stoppedNote(ctx, stopRequester(ctx, t))
	body := note
	if p := strings.TrimSpace(partial); p != "" {
		body += "\n\nHere's what I'd done up to that point:\n\n" + p
	}
	if postStatus != nil {
		postStatus(body)
	}
	if notify != nil {
		notify(note)
	}
	commitAgentTaskState(ctx, t.Id, "cancel (stopped by a person)", func(c context.Context) error {
		return model.CancelLeasedAgentTask(c, t.Id, lease, note, partial, runID)
	})
	return true
}

// stopRequester resolves who asked for the stop. The claimed row carries it when
// the request predates the claim; otherwise it is read back on a detached context
// (the run context is cancelled by the stop itself). Nil when unknown, which the
// message handles.
func stopRequester(ctx context.Context, t *model.AgentTask) *uuid.UUID {
	if t == nil {
		return nil
	}
	if t.CancelRequestedBy != nil {
		return t.CancelRequestedBy
	}
	fresh, err := model.GetAgentTaskByID(context.WithoutCancel(ctx), t.Id)
	if err != nil || fresh == nil {
		return nil
	}
	return fresh.CancelRequestedBy
}
