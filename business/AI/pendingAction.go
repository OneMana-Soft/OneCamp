package business

// Durable in-thread write approvals.
//
// This is the "act" half of the AI teammate: when the assistant / a bot / an
// agent proposes a WRITE action, we persist it as a durable PendingAction
// instead of popping an ephemeral client dialog. The proposal survives tab
// close, navigation and reload, and is surfaced as an in-thread Approve/Deny
// card (reconciled on load from ListOpenPendingActions).
//
// Security model (no confused-deputy):
//   - The bot holds NO standalone write privilege.
//   - An action executes ONLY after a human approves it, and executes AS that
//     human (ExecuteAction runs the executor as approverInfo's Dgraph uuid, so
//     the user's permissions are re-checked at execution time).
//   - v1: requester == approver. The approve handler enforces this.
//
// At-most-once execution: ApprovePendingAction claims the row with a guarded
// pending->executing transition (TransitionToExecuting). Of N concurrent
// approvals (double-click / multi-tab / multi-device) exactly one claims it and
// runs; the rest no-op and reconcile. The TTL is enforced in the same guarded
// UPDATE, so an expired card can never execute.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// pendingActionTTL is how long a proposed write stays approvable before it
// expires. Long enough to survive a meeting / context switch, short enough that
// a stale proposal (its grounding may have changed) does not linger forever.
const pendingActionTTL = 24 * time.Hour

// Errors surfaced to the controller for precise HTTP mapping.
var (
	ErrPendingActionNotFound  = errors.New("pending action not found")
	ErrPendingActionForbidden = errors.New("not authorized to act on this pending action")
	ErrPendingActionResolved  = errors.New("pending action already resolved")
	ErrPendingActionExpired   = errors.New("pending action expired")
)

// CreatePendingAction persists a proposed write as a durable, approvable record
// and notifies the requester (so the in-thread card appears live). idempotencyKey
// may be empty; when set, a retried proposal for the same logical write returns
// the existing record instead of stacking a duplicate card.
//
// The action is validated up front so a malformed proposal never becomes a
// card (it would only fail later at approval time anyway).
func CreatePendingAction(ctx context.Context, requestedBy uuid.UUID, surfaceType, surfaceID, toolName string, params map[string]string, description, idempotencyKey string, attribution ...pendingModels.Attribution) (*pendingModels.PendingAction, error) {
	// Variadic because only the agent runner has an agent to name, and the
	// assistant, the coworker and the MCP bridge should not each grow two
	// arguments they would always pass nil for.
	var attr pendingModels.Attribution
	if len(attribution) > 0 {
		attr = attribution[0]
	}
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return nil, fmt.Errorf("tool_name is required")
	}
	if params == nil {
		params = map[string]string{}
	}
	// Validate against the same executor registry that approval will use, so a
	// proposal that could never run is rejected at creation.
	if err := ai.ValidateAction(ai.ProposedAction{ToolName: toolName, Params: params}); err != nil {
		return nil, fmt.Errorf("invalid action: %w", err)
	}

	action, err := pendingModels.CreatePendingAction(ctx, requestedBy, surfaceType, surfaceID, toolName, params, description, idempotencyKey, time.Now().Add(pendingActionTTL), attr)
	if err != nil {
		return nil, err
	}
	if action == nil {
		return nil, fmt.Errorf("failed to create pending action")
	}

	enrichRisk(action)
	publishPendingAction(action.RequestedBy.String(), &mqttStruct.MqttPendingAction{
		Action:      "created",
		ID:          action.Id.String(),
		SurfaceType: action.SurfaceType,
		SurfaceID:   action.SurfaceID,
		ToolName:    action.ToolName,
		Description: action.Description,
		Destructive: action.Destructive,
		Status:      action.Status,
		CreatedAt:   action.CreatedAt.Format(time.RFC3339),
	})
	if action.Fresh {
		notifyApprovalNeeded(action)
	}
	return action, nil
}

// enrichRisk annotates an action with derived, non-persisted risk metadata so
// the FE can flag it (currently: whether its tool is a destructive/irreversible
// write per the live tool registry). Kept generic and centralized so every
// return path (create / list / resolve) presents the same signal. Nil-safe.
//
// The signal asks ToolNeedsHumanBeforeUnattended, not ToolIsDestructive, because
// the question this card puts to a person is "can I undo this after I click
// Approve?" — and the answer is no both for a tool that destroys state and for one
// whose effect has left the workspace. Flagging only the former would have shown a
// plain card for "send this email", the least undoable thing on the list.
func enrichRisk(actions ...*pendingModels.PendingAction) {
	for _, a := range actions {
		if a != nil {
			a.Destructive = ai.ToolNeedsHumanBeforeUnattended(a.ToolName)
		}
	}
}

// ListOpenPendingActions returns the caller's open (pending, unexpired)
// approvals, newest first. The FE calls this on load to re-render any card that
// outlived the tab that created it.
func ListOpenPendingActions(ctx context.Context, userUUID uuid.UUID) ([]*pendingModels.PendingAction, error) {
	actions, err := pendingModels.ListOpenByUser(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	enrichRisk(actions...)
	return actions, nil
}

// ApprovePendingAction is the keystone: a human approves a proposed write and
// it executes AS them, at most once, with their permissions re-checked.
//
// Flow:
//  1. Load + authorize (only the requester may approve; v1 requester==approver).
//  2. Guarded claim (pending->executing) — the idempotency + TTL gate.
//  3. Execute via ExecuteAction AS the approver (permissions re-checked there).
//  4. Finalize (executed|failed) with the outcome + notify live.
func ApprovePendingAction(ctx context.Context, approverInfo *userModels.UserInfo, id uuid.UUID) (*pendingModels.PendingAction, error) {
	if approverInfo == nil {
		return nil, ErrPendingActionForbidden
	}
	approverUUID, err := uuid.Parse(approverInfo.UserDgraphInfo.Uuid)
	if err != nil {
		return nil, ErrPendingActionForbidden
	}

	action, err := pendingModels.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if action == nil {
		return nil, ErrPendingActionNotFound
	}
	// Authorization: only the requester may approve their own action (v1).
	if action.RequestedBy != approverUUID {
		return nil, ErrPendingActionForbidden
	}
	// Fast-path rejections before claiming, for precise client messaging.
	if action.Status != pendingModels.StatusPending {
		return action, ErrPendingActionResolved
	}
	if action.Expired() {
		return action, ErrPendingActionExpired
	}

	// Guarded claim: exactly one concurrent approver wins; the gate also
	// enforces the TTL atomically.
	claimed, err := pendingModels.TransitionToExecuting(ctx, id)
	if err != nil {
		return nil, err
	}
	if !claimed {
		// Lost the race or expired between the read and the claim. Reload so
		// the caller gets the authoritative terminal/expired state.
		latest, gerr := pendingModels.GetByID(ctx, id)
		if gerr == nil && latest != nil {
			if latest.Expired() && latest.Status == pendingModels.StatusPending {
				return latest, ErrPendingActionExpired
			}
			return latest, ErrPendingActionResolved
		}
		return action, ErrPendingActionResolved
	}

	// Execute AS the approver. ExecuteAction (or the plan executor for a
	// multi-step plan) re-validates and runs as the approver's Dgraph uuid
	// (permissions re-checked), sanitizing any error.
	resp, execErr := approveExecute(ctx, approverInfo, action)

	status := pendingModels.StatusExecuted
	var resultText, errText string
	switch {
	case execErr != nil:
		// Hard failure (service disabled, no executor, validation): record it.
		status = pendingModels.StatusFailed
		errText = execErr.Error()
	case resp == nil || !resp.Success:
		// Executor ran but reported failure (already sanitized message).
		status = pendingModels.StatusFailed
		if resp != nil {
			errText = resp.Message
		} else {
			errText = "Action failed"
		}
	default:
		resultText = resp.Message
	}

	if ferr := pendingModels.Finalize(ctx, id, status, resultText, errText, approverUUID); ferr != nil {
		helpers.LogErrorWithContext(ctx, "business/ApprovePendingAction finalize err: %+v", ferr)
		// The action DID execute; the record just failed to persist its
		// terminal state. Reflect the real outcome in the returned value so
		// the user is not told it failed when it did not.
	}

	final, _ := pendingModels.GetByID(ctx, id)
	if final == nil {
		// Synthesize from what we know so the caller still gets a result.
		final = action
		final.Status = status
		if resultText != "" {
			final.Result = &resultText
		}
		if errText != "" {
			final.Error = &errText
		}
	}

	enrichRisk(final)
	publishPendingAction(final.RequestedBy.String(), resolvedMsg(final))
	return final, nil
}

// RejectPendingAction denies a proposed write (human said no). Only the
// requester may reject; idempotent on an already-resolved action.
func RejectPendingAction(ctx context.Context, approverInfo *userModels.UserInfo, id uuid.UUID) (*pendingModels.PendingAction, error) {
	if approverInfo == nil {
		return nil, ErrPendingActionForbidden
	}
	approverUUID, err := uuid.Parse(approverInfo.UserDgraphInfo.Uuid)
	if err != nil {
		return nil, ErrPendingActionForbidden
	}

	action, err := pendingModels.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if action == nil {
		return nil, ErrPendingActionNotFound
	}
	if action.RequestedBy != approverUUID {
		return nil, ErrPendingActionForbidden
	}
	if action.Status != pendingModels.StatusPending {
		return action, ErrPendingActionResolved
	}

	if rerr := pendingModels.Reject(ctx, id, approverUUID); rerr != nil {
		// Lost a race to another resolution; reload + report resolved.
		latest, gerr := pendingModels.GetByID(ctx, id)
		if gerr == nil && latest != nil {
			return latest, ErrPendingActionResolved
		}
		return nil, rerr
	}

	final, _ := pendingModels.GetByID(ctx, id)
	if final == nil {
		final = action
		final.Status = pendingModels.StatusRejected
	}
	enrichRisk(final)
	publishPendingAction(final.RequestedBy.String(), resolvedMsg(final))
	return final, nil
}

// resolvedMsg builds the live "resolved" payload for a terminal action.
func resolvedMsg(a *pendingModels.PendingAction) *mqttStruct.MqttPendingAction {
	msg := &mqttStruct.MqttPendingAction{
		Action:      "resolved",
		ID:          a.Id.String(),
		SurfaceType: a.SurfaceType,
		SurfaceID:   a.SurfaceID,
		ToolName:    a.ToolName,
		Description: a.Description,
		Destructive: ai.ToolNeedsHumanBeforeUnattended(a.ToolName),
		Status:      a.Status,
	}
	if a.Result != nil {
		msg.Result = *a.Result
	}
	if a.Error != nil {
		msg.Error = *a.Error
	}
	return msg
}

func publishPendingAction(userUUID string, msg *mqttStruct.MqttPendingAction) {
	mqttBusiness.PublishMessageToUser(userUUID, mqttStruct.MESSAGE_AI_PENDING_ACTION, msg)
}
