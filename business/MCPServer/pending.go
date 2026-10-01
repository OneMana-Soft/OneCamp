package business

// The approval store, wired to the policy.
//
// write.go decides WHAT should happen to a mutating call and deliberately performs no
// I/O: PlanWrite takes a checkExisting function so the policy stays testable without a
// database and so the store behind it can change without touching the rules. This file
// is the other half — the two functions that connect that policy to ai_pending_actions.
//
// WHY THE APPROVAL CARD IS THE SAME ONE THE APP ALREADY USES. OneCamp already has an
// in-thread Approve/Deny card for actions an in-app agent proposes: a row in
// ai_pending_actions with a TTL, a unique index on its idempotency key, MQTT delivery
// to the surface, and a guarded pending -> executing claim so two approvers cannot both
// execute it. Building a second approval mechanism for MCP would mean a second place
// where "has a human agreed to this" is answered, and the two would drift — the same
// argument that made a second MCP server the wrong answer.
//
// So an external agent's destructive proposal becomes exactly the card a colleague's
// proposal becomes. The reviewer does not need to know or care that this one arrived
// over MCP, which is the property worth having: approving agent work should not require
// understanding agent plumbing.

import (
	"context"
	"strings"

	pendingBusiness "github.com/akashc777/OneCamp/business/AI"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	"github.com/google/uuid"
)

// CheckExistingWrite is the checkExisting function PlanWrite expects, backed by
// ai_pending_actions.
//
// The status mapping is the interesting part, because the two vocabularies are not
// identical and the differences are decisions:
//
//   - executing and executed both mean APPLIED. A call that is mid-execution must not
//     be started again by a retry arriving a moment later; treating "in progress" as
//     "not yet done" is exactly how a duplicate write happens.
//   - failed means NONE, so a retry is allowed. A write that errored did not take
//     effect, and refusing forever would leave an agent unable to recover from a
//     transient fault.
//   - anything unrecognised means AWAITING, which is the conservative reading: it
//     defers to a human rather than proceeding or silently reporting success.
func CheckExistingWrite(ctx context.Context, idempotencyKey string) (ExistingWrite, error) {
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		return ExistingWrite{State: ExistingNone}, nil
	}

	row, err := pendingModels.GetByIdempotencyKey(ctx, key)
	if err != nil {
		// Surfaced rather than swallowed. PlanWrite refuses on a lookup error,
		// because applying a write twice is worse than not applying it and the caller
		// can retry once the store is healthy.
		return ExistingWrite{}, err
	}
	if row == nil {
		return ExistingWrite{State: ExistingNone}, nil
	}

	out := ExistingWrite{PendingActionID: row.Id.String()}
	switch row.Status {
	case pendingModels.StatusPending:
		out.State = ExistingAwaitingApproval
	case pendingModels.StatusExecuting, pendingModels.StatusExecuted:
		out.State = ExistingApplied
	case pendingModels.StatusRejected:
		out.State = ExistingRejected
	case pendingModels.StatusExpired:
		out.State = ExistingExpired
	case pendingModels.StatusFailed:
		// Did not take effect, so the key is free again.
		out.State = ExistingNone
	default:
		out.State = ExistingAwaitingApproval
	}
	return out, nil
}

// RequestApproval turns a destructive proposal into the in-thread Approve/Deny card a
// human will act on, and returns its id.
//
// ATTRIBUTED TO THE PRINCIPAL, NOT THE AGENT. requested_by is the human whose
// credential authorised the call, which is what makes the card answerable: a reviewer
// can ask that person why. Attributing it to the agent would produce a card requested
// by software with nobody to ask, and the agent is still named in the description so
// nothing is lost.
//
// TWO INDEPENDENT PEOPLE, BY CONSTRUCTION. The person who approves is whoever is
// looking at the surface, and it is not this caller — the caller is an external process
// with no session. So a destructive write over MCP requires the principal to have
// authorised the credential and a second human to approve the specific action, without
// either step having to remember the other.
func RequestApproval(ctx context.Context, spec *ToolSpec, call ToolCallContext, clientName, idempotencyKey string) (string, error) {
	principal, err := uuid.Parse(strings.TrimSpace(call.PrincipalUserID))
	if err != nil {
		// No accountable person means no card anyone could act on.
		return "", err
	}

	// Reuses the business-layer creator rather than the model directly, because it
	// validates the proposal against the same executor registry approval will use — so
	// a proposal that could never run is refused now instead of failing in front of
	// whoever approved it.
	// A DECLARED SURFACE, not the resource kind — see surface.go. This is the call that
	// makes the difference visible: the card has to land somewhere a person will look, and
	// for a chat write that is the conversation itself rather than the home list.
	surfaceType, surfaceID := SurfaceForResource(call.Resource, call.PrincipalUserID)

	action, err := pendingBusiness.CreatePendingAction(
		ctx,
		principal,
		surfaceType,
		surfaceID,
		spec.Name,
		StringifyArgs(call.Args),
		ApprovalDescription(spec, call, clientName),
		idempotencyKey,
	)
	if err != nil {
		return "", err
	}
	return action.Id.String(), nil
}
