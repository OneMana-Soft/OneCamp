package business

// CRUD + validation for workflows. Admin-managed (workspace automation is an
// admin concern, like webhooks and slash commands). Every write reloads the
// in-memory engine so changes take effect immediately without a restart.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// Limits keep a single rule sane and bound the engine's per-event work.
const (
	maxNameLen    = 120
	maxKeywords   = 25
	maxKeywordLen = 80
	maxReplyLen   = 4000
	maxBotNameLen = 80
)

// WorkflowInput is the create/update payload from the admin controller.
type WorkflowInput struct {
	Name          string
	IsActive      bool
	TriggerType   string                 // defaults to message_posted when empty
	TriggerConfig map[string]interface{} // per-trigger params
	BotName       string                 // optional per-workflow bot label
	ChannelID     string                 // optional; "" = any channel
	Keywords      []string
	MatchType     string // any | all
	Actions       []WorkflowAction
}

// validate normalizes and checks an input, returning the channel UUID (or nil)
// validatedWorkflow holds the normalized, persist-ready form of an input.
type validatedWorkflow struct {
	channelID         *uuid.UUID
	triggerType       string
	triggerConfigJSON string
	botName           *string
	keywordsJSON      string
	actionsJSON       string
}

// validate normalizes and checks an input, returning the persist-ready form.
func validate(in *WorkflowInput) (*validatedWorkflow, error) {
	out := &validatedWorkflow{}

	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if len(in.Name) > maxNameLen {
		return nil, fmt.Errorf("name is too long (max %d)", maxNameLen)
	}

	// Trigger type (default to message_posted for back-compat / empty input).
	out.triggerType = strings.TrimSpace(in.TriggerType)
	if out.triggerType == "" {
		out.triggerType = workflowModel.TriggerMessagePosted
	}
	if !workflowModel.ValidTriggerType(out.triggerType) {
		return nil, fmt.Errorf("unsupported trigger type %q", out.triggerType)
	}
	// Reject triggers the engine doesn't actively evaluate yet, so an admin
	// can't create a workflow that silently never runs.
	if !IsEngineTrigger(out.triggerType) {
		return nil, fmt.Errorf("trigger type %q is not available yet", out.triggerType)
	}

	// Trigger config (optional JSON object). Marshal defensively; default {}.
	if len(in.TriggerConfig) == 0 {
		out.triggerConfigJSON = "{}"
	} else {
		b, merr := json.Marshal(in.TriggerConfig)
		if merr != nil {
			return nil, fmt.Errorf("invalid trigger config")
		}
		out.triggerConfigJSON = string(b)
	}

	// Bot label (optional per-workflow override).
	if bn := strings.TrimSpace(in.BotName); bn != "" {
		if len(bn) > maxBotNameLen {
			return nil, fmt.Errorf("bot name too long (max %d)", maxBotNameLen)
		}
		out.botName = &bn
	}

	// Channel scope (optional). Keyword triggers (message_posted) may scope to
	// a channel; non-message triggers may also be channel-scoped where it makes
	// sense (e.g. user_joined_channel). An empty value means "any".
	if strings.TrimSpace(in.ChannelID) != "" {
		id, perr := uuid.Parse(strings.TrimSpace(in.ChannelID))
		if perr != nil {
			return nil, fmt.Errorf("invalid channel_id")
		}
		out.channelID = &id
	}

	// Keywords (only meaningful for message_posted, but harmless to store).
	if len(in.Keywords) > maxKeywords {
		return nil, fmt.Errorf("too many keywords (max %d)", maxKeywords)
	}
	cleanKeywords := make([]string, 0, len(in.Keywords))
	for _, kw := range in.Keywords {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		if len(kw) > maxKeywordLen {
			return nil, fmt.Errorf("keyword too long (max %d)", maxKeywordLen)
		}
		cleanKeywords = append(cleanKeywords, kw)
	}

	// Match type.
	if in.MatchType != workflowModel.MatchAll {
		in.MatchType = workflowModel.MatchAny
	}

	// Actions.
	if len(in.Actions) == 0 {
		return nil, fmt.Errorf("at least one action is required")
	}
	if len(in.Actions) > MaxActionsPerWorkflow {
		return nil, fmt.Errorf("too many actions (max %d)", MaxActionsPerWorkflow)
	}
	for i := range in.Actions {
		act := &in.Actions[i]
		switch act.Type {
		case ActionReply, ActionReplyEphemeral:
			act.Text = strings.TrimSpace(act.Text)
			if act.Text == "" {
				return nil, fmt.Errorf("reply action requires text")
			}
			if len(act.Text) > maxReplyLen {
				return nil, fmt.Errorf("reply text too long (max %d)", maxReplyLen)
			}
		case ActionCreateTask:
			act.ProjectID = strings.TrimSpace(act.ProjectID)
			if act.ProjectID == "" {
				return nil, fmt.Errorf("create_task action requires project_id")
			}
			if _, perr := uuid.Parse(act.ProjectID); perr != nil {
				return nil, fmt.Errorf("create_task action has invalid project_id")
			}
			act.TaskName = strings.TrimSpace(act.TaskName)
			// Only message_posted carries triggering text to default the task
			// name from. For other triggers an explicit task name is required.
			if act.TaskName == "" && out.triggerType != workflowModel.TriggerMessagePosted {
				return nil, fmt.Errorf("create_task action requires a task name for this trigger")
			}
			act.Priority = strings.ToLower(strings.TrimSpace(act.Priority))
			switch act.Priority {
			case "", "low", "medium", "high":
				// ok (empty → engine defaults to medium)
			default:
				return nil, fmt.Errorf("create_task priority must be low, medium, or high")
			}
		case ActionWarnUser:
			act.Text = strings.TrimSpace(act.Text)
			if act.Text == "" {
				return nil, fmt.Errorf("warn_user action requires text")
			}
			if len(act.Text) > maxReplyLen {
				return nil, fmt.Errorf("warn_user text too long (max %d)", maxReplyLen)
			}
		case ActionDeleteMessage:
			// Deletes the triggering message — only the message_posted trigger
			// carries one. Reject on other triggers so the rule isn't a silent
			// no-op.
			if out.triggerType != workflowModel.TriggerMessagePosted {
				return nil, fmt.Errorf("delete_message is only available for the 'message posted' trigger")
			}
		case ActionFlagToChannel:
			act.TargetChannelID = strings.TrimSpace(act.TargetChannelID)
			if act.TargetChannelID == "" {
				return nil, fmt.Errorf("flag_to_channel action requires a target channel")
			}
			if _, perr := uuid.Parse(act.TargetChannelID); perr != nil {
				return nil, fmt.Errorf("flag_to_channel action has invalid target channel")
			}
			if out.triggerType != workflowModel.TriggerMessagePosted {
				return nil, fmt.Errorf("flag_to_channel is only available for the 'message posted' trigger")
			}
		default:
			return nil, fmt.Errorf("unknown action type %q", act.Type)
		}
	}

	kwBytes, _ := json.Marshal(cleanKeywords)
	actBytes, _ := json.Marshal(in.Actions)
	out.keywordsJSON = string(kwBytes)
	out.actionsJSON = string(actBytes)
	return out, nil
}

// Actor is the user performing a workflow management action, with the
// authority bits needed for ownership/permission decisions.
type Actor struct {
	UserID  uuid.UUID
	IsAdmin bool
}

// canManage reports whether the actor may edit/toggle/delete a given workflow.
// Admins manage any workflow; a non-admin manages only the ones they created.
func canManage(a Actor, wf *workflowModel.Workflow) bool {
	if wf == nil {
		return false
	}
	if a.IsAdmin {
		return true
	}
	return wf.CreatedBy == a.UserID
}

// errForbidden signals the actor isn't allowed to manage the target workflow.
var errForbidden = fmt.Errorf("not authorized to manage this workflow")

// errNotFound signals the workflow doesn't exist (or was deleted).
var errNotFound = fmt.Errorf("workflow not found")

// CreateWorkflow validates, persists, and hot-reloads a new workflow.
func CreateWorkflow(ctx context.Context, in WorkflowInput, createdBy uuid.UUID) (*workflowModel.Workflow, error) {
	if err := normalizeTaskStatusTrigger(ctx, &in); err != nil {
		return nil, err
	}
	v, err := validate(&in)
	if err != nil {
		return nil, err
	}
	id, err := workflowModel.CreateWorkflow(ctx, in.Name, createdBy, v.triggerType, v.triggerConfigJSON, v.botName, v.channelID, v.keywordsJSON, in.MatchType, v.actionsJSON)
	if err != nil {
		return nil, err
	}
	// Persist is_active=true default; honor explicit "create disabled".
	if !in.IsActive {
		if serr := workflowModel.SetWorkflowActive(ctx, id, false); serr != nil {
			helpers.LogErrorWithContext(ctx, "Workflow/CreateWorkflow set inactive err: %v", serr)
		}
	}
	_ = reload(ctx)
	return workflowModel.GetWorkflowByID(ctx, id)
}

// UpdateWorkflow validates, persists edits, and hot-reloads. The actor must be
// allowed to manage the target workflow (admin, or its creator).
func UpdateWorkflow(ctx context.Context, id uuid.UUID, in WorkflowInput, actor Actor) (*workflowModel.Workflow, error) {
	existing, err := workflowModel.GetWorkflowByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, errNotFound
	}
	if !canManage(actor, existing) {
		return nil, errForbidden
	}
	if err := normalizeTaskStatusTrigger(ctx, &in); err != nil {
		return nil, err
	}
	v, err := validate(&in)
	if err != nil {
		return nil, err
	}
	if err := workflowModel.UpdateWorkflow(ctx, id, in.Name, in.IsActive, v.triggerType, v.triggerConfigJSON, v.botName, v.channelID, v.keywordsJSON, in.MatchType, v.actionsJSON); err != nil {
		return nil, err
	}
	_ = reload(ctx)
	return workflowModel.GetWorkflowByID(ctx, id)
}

// SetActive toggles a workflow and hot-reloads, enforcing manage permission.
func SetActive(ctx context.Context, id uuid.UUID, isActive bool, actor Actor) error {
	existing, err := workflowModel.GetWorkflowByID(ctx, id)
	if err != nil {
		return err
	}
	if existing == nil {
		return errNotFound
	}
	if !canManage(actor, existing) {
		return errForbidden
	}
	if err := workflowModel.SetWorkflowActive(ctx, id, isActive); err != nil {
		return err
	}
	_ = reload(ctx)
	return nil
}

// DeleteWorkflow soft-deletes a workflow and hot-reloads, enforcing manage
// permission.
func DeleteWorkflow(ctx context.Context, id uuid.UUID, actor Actor) error {
	existing, err := workflowModel.GetWorkflowByID(ctx, id)
	if err != nil {
		return err
	}
	if existing == nil {
		return errNotFound
	}
	if !canManage(actor, existing) {
		return errForbidden
	}
	if err := workflowModel.SoftDeleteWorkflow(ctx, id); err != nil {
		return err
	}
	_ = reload(ctx)
	return nil
}

// ListWorkflows returns workflows visible to the actor: admins see all,
// members see only the ones they created.
func ListWorkflows(ctx context.Context, actor Actor) ([]*workflowModel.Workflow, error) {
	if actor.IsAdmin {
		return workflowModel.ListWorkflows(ctx)
	}
	return workflowModel.ListWorkflowsByCreator(ctx, actor.UserID)
}

// GetWorkflow returns a single workflow by id if the actor may view it.
func GetWorkflow(ctx context.Context, id uuid.UUID, actor Actor) (*workflowModel.Workflow, error) {
	wf, err := workflowModel.GetWorkflowByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if wf == nil {
		return nil, nil
	}
	if !canManage(actor, wf) {
		return nil, errForbidden
	}
	return wf, nil
}

// IsForbidden / IsNotFound let the controller map business errors to HTTP codes.
func IsForbidden(err error) bool { return err == errForbidden }
func IsNotFound(err error) bool  { return err == errNotFound }
