package business

// Plan-approve execution: an agent in "plan" autonomy proposes its FULL ordered
// plan of writes as a SINGLE durable approval (one card) instead of one card per
// write. On approval the whole plan executes step-by-step AS the approver, each
// step's permissions re-checked exactly as a per-action approval would be — so
// bundling adds no privilege, only "approve once, no surprises". This rides the
// existing pending-action surface (tray + MQTT + reconcile) unchanged; a plan is
// just a pending action whose tool is a sentinel and whose params carry the
// ordered steps.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// planSentinelTool marks a pending action that carries a multi-step agent plan
// rather than a single tool call. It is never a real registered tool, so it is
// invisible to the tool registry / MCP / API; only the approval path handles it.
const planSentinelTool = "__agent_plan__"

// PlanStep is one write in an agent's proposed plan.
type PlanStep struct {
	ToolName    string            `json:"tool_name"`
	Params      map[string]string `json:"params"`
	Description string            `json:"description"`
}

// BuildPlanSummary renders the ordered plan as a numbered, human-readable list
// for the approval card. Pure.
func BuildPlanSummary(agentName string, steps []PlanStep) string {
	name := strings.TrimSpace(agentName)
	if name == "" {
		name = "Agent"
	}
	var b strings.Builder
	if len(steps) == 1 {
		b.WriteString(name + " proposes 1 step — approve to run it:")
	} else {
		b.WriteString(fmt.Sprintf("%s proposes a %d-step plan — approve to run all:", name, len(steps)))
	}
	for i, s := range steps {
		d := strings.TrimSpace(s.Description)
		if d == "" {
			d = strings.TrimSpace(s.ToolName)
		}
		b.WriteString(fmt.Sprintf("\n%d. %s", i+1, d))
	}
	return b.String()
}

func encodePlanSteps(steps []PlanStep) (string, error) {
	b, err := json.Marshal(steps)
	return string(b), err
}

func decodePlanSteps(s string) ([]PlanStep, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var steps []PlanStep
	err := json.Unmarshal([]byte(s), &steps)
	return steps, err
}

// CreatePlanAction persists an agent's proposed plan as a SINGLE durable
// approval. Each step is validated against the executor registry up front (so a
// plan that could never run is rejected at creation, like CreatePendingAction),
// then the ordered steps are stored under the sentinel tool. Notifies the
// requester so the card appears live.
func CreatePlanAction(ctx context.Context, requestedBy uuid.UUID, surfaceType, surfaceID string, steps []PlanStep, summary string, attribution ...pendingModels.Attribution) (*pendingModels.PendingAction, error) {
	// Approving a plan is the same signal as approving a single action, so it
	// is attributed the same way. See CreatePendingAction.
	var attr pendingModels.Attribution
	if len(attribution) > 0 {
		attr = attribution[0]
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("a plan needs at least one step")
	}
	for _, s := range steps {
		if err := ai.ValidateAction(ai.ProposedAction{ToolName: s.ToolName, Params: s.Params}); err != nil {
			return nil, fmt.Errorf("invalid plan step %q: %w", s.ToolName, err)
		}
	}
	enc, err := encodePlanSteps(steps)
	if err != nil {
		return nil, fmt.Errorf("failed to encode plan")
	}
	action, err := pendingModels.CreatePendingAction(ctx, requestedBy, surfaceType, surfaceID,
		planSentinelTool, map[string]string{"steps": enc}, summary, "", time.Now().Add(pendingActionTTL), attr)
	if err != nil {
		return nil, err
	}
	if action == nil {
		return nil, fmt.Errorf("failed to create plan action")
	}
	publishPendingAction(action.RequestedBy.String(), &mqttStruct.MqttPendingAction{
		Action:      "created",
		ID:          action.Id.String(),
		SurfaceType: action.SurfaceType,
		SurfaceID:   action.SurfaceID,
		ToolName:    action.ToolName,
		Description: action.Description,
		Status:      action.Status,
		CreatedAt:   action.CreatedAt.Format(time.RFC3339),
	})
	return action, nil
}

// approveExecute runs an approved pending action: a normal single tool, or a
// multi-step agent plan (sentinel tool). Both execute AS the approver with
// per-step permission re-checks. Used by ApprovePendingAction so the
// security-critical approve flow has one execution entry point.
func approveExecute(ctx context.Context, approverInfo *userModels.UserInfo, action *pendingModels.PendingAction) (*adapter.ExecuteActionResponse, error) {
	// Which agent proposed this, from the stored row: a tool that reports back
	// where its run was asked (code_pr) needs it, and the approval request has
	// nothing else to say so.
	if action.AgentID != nil {
		ctx = ai.WithApprovedAgentProposal(ctx, action.AgentID.String())
	}
	if action.ToolName == planSentinelTool {
		return executePlanSteps(ctx, approverInfo, action.Params)
	}
	return ExecuteAction(ctx, approverInfo, action.ToolName, action.Params, nil)
}

// executePlanSteps runs an approved plan's steps in order AS the approver,
// stopping at the first failure (and reporting how many earlier steps applied,
// so a partial outcome is honest). Each step goes through ExecuteAction, so its
// permissions are re-checked exactly as a standalone approval.
func executePlanSteps(ctx context.Context, approverInfo *userModels.UserInfo, params map[string]string) (*adapter.ExecuteActionResponse, error) {
	steps, err := decodePlanSteps(params["steps"])
	if err != nil || len(steps) == 0 {
		return &adapter.ExecuteActionResponse{Success: false, Message: "This plan could not be read."}, nil
	}
	done := 0
	for i, s := range steps {
		resp, eerr := ExecuteAction(ctx, approverInfo, s.ToolName, s.Params, nil)
		if eerr != nil {
			return &adapter.ExecuteActionResponse{
				Success: false,
				Message: fmt.Sprintf("Step %d failed: %s (%d earlier step(s) applied).", i+1, eerr.Error(), done),
			}, nil
		}
		if resp == nil || !resp.Success {
			msg := "the step could not be completed"
			if resp != nil && resp.Message != "" {
				msg = resp.Message
			}
			return &adapter.ExecuteActionResponse{
				Success: false,
				Message: fmt.Sprintf("Step %d failed: %s (%d earlier step(s) applied).", i+1, msg, done),
			}, nil
		}
		done++
	}
	return &adapter.ExecuteActionResponse{Success: true, Message: fmt.Sprintf("Plan complete — %d step(s) executed.", done)}, nil
}
