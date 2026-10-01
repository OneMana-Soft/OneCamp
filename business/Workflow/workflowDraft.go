package business

// Natural-language workflow authoring: turn a plain-English description into a
// DRAFT workflow the user reviews and saves through the normal validated path.
// The draft is NEVER persisted here — it only pre-fills the builder form, so
// the existing create-time validation (and the user's own review) remains the
// single source of truth. The model is explicitly told not to invent ids
// (channels/projects); those are blanked out and the user picks them in the UI.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// WorkflowDraft is the model-produced scaffold returned to the builder. Field
// names mirror the saved-workflow JSON the FE already consumes, so the form
// fills from it directly. Ids are intentionally absent/blank (the user selects
// real channels/projects in the form).
type WorkflowDraft struct {
	Name        string           `json:"name"`
	TriggerType string           `json:"trigger_type"`
	Keywords    []string         `json:"keywords"`
	MatchType   string           `json:"match_type"`
	Actions     []WorkflowAction `json:"actions"`
	// TriggerConfig is task_status_changed's {"to_status": "<a status as
	// said, e.g. QA>"}. The form maps the name to the project's status once
	// the person picks the project; no id ever comes from the model.
	TriggerConfig map[string]string `json:"trigger_config,omitempty"`
	// Notes is a short, human-readable explanation of what still needs to be
	// filled in (e.g. "pick the project for the task"), shown under the form.
	Notes string `json:"notes,omitempty"`
}

// DraftWorkflow generates a draft from a natural-language prompt. Runs as the
// requesting user (rate/breaker bound); returns a friendly error when AI is off
// or the model output can't be understood.
func DraftWorkflow(ctx context.Context, userUUID, prompt string) (*WorkflowDraft, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, fmt.Errorf("describe the automation you want")
	}
	if len(prompt) > 2000 {
		prompt = prompt[:2000]
	}

	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if llm == nil {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	if err := cb.Allow(); err != nil {
		return nil, err
	}
	if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		return nil, err
	}

	out, err := ai.ChatJSONWithRetry(ctx, llm, cb, draftSystemPrompt(), "Describe the automation:\n"+prompt,
		ai.ChatOptions{Temperature: 0.2, MaxTokens: 900},
		func(s string) bool { _, e := parseWorkflowDraft(s); return e == nil })
	if err != nil {
		helpers.LogErrorWithContext(ctx, "DraftWorkflow chat err: %v", err)
		return nil, fmt.Errorf("could not draft the workflow")
	}

	draft, perr := parseWorkflowDraft(out)
	if perr != nil {
		helpers.LogErrorWithContext(ctx, "DraftWorkflow parse err: %v (raw=%.300s)", perr, out)
		return nil, fmt.Errorf("could not understand that; try describing it differently")
	}
	return draft, nil
}

// parseWorkflowDraft extracts the JSON object from the model output and
// normalizes it into a safe draft: only known trigger/action types survive, and
// any ids the model may have invented are stripped (the user fills them in).
func parseWorkflowDraft(raw string) (*WorkflowDraft, error) {
	js := extractJSONObject(raw)
	if js == "" {
		return nil, fmt.Errorf("no JSON object found")
	}
	var d WorkflowDraft
	if err := json.Unmarshal([]byte(js), &d); err != nil {
		return nil, err
	}

	d.Name = strings.TrimSpace(d.Name)

	// Trigger type: only the engine-supported kinds; default to message_posted.
	d.TriggerType = strings.TrimSpace(d.TriggerType)
	if !IsEngineTrigger(d.TriggerType) {
		d.TriggerType = workflowModel.TriggerMessagePosted
	}

	// Trigger config: a status name for task_status_changed, nothing else.
	cfg := map[string]string{}
	if d.TriggerType == workflowModel.TriggerTaskStatusChanged {
		if to := strings.TrimSpace(d.TriggerConfig["to_status"]); to != "" && len(to) <= 40 {
			cfg["to_status"] = to
		}
	}
	d.TriggerConfig = cfg

	// Match type: any | all.
	if d.MatchType != workflowModel.MatchAll {
		d.MatchType = workflowModel.MatchAny
	}

	// Keep only non-empty keywords.
	keywords := make([]string, 0, len(d.Keywords))
	for _, k := range d.Keywords {
		if k = strings.TrimSpace(k); k != "" {
			keywords = append(keywords, k)
		}
	}
	d.Keywords = keywords

	// Filter actions to known types and strip any ids the model may have
	// fabricated (channels/projects must be chosen by the user in the form).
	needsTarget := false
	actions := make([]WorkflowAction, 0, len(d.Actions))
	for _, a := range d.Actions {
		a.Type = strings.TrimSpace(a.Type)
		if !isKnownActionType(a.Type) {
			continue
		}
		if a.ProjectID != "" || a.TargetChannelID != "" {
			needsTarget = true
		}
		a.ProjectID = ""       // user picks the project
		a.TargetChannelID = "" // user picks the review channel
		actions = append(actions, a)
	}
	d.Actions = actions

	if len(d.Actions) == 0 {
		return nil, fmt.Errorf("no usable actions")
	}

	if needsTarget && d.Notes == "" {
		d.Notes = "Pick the target channel or project for the action(s) before saving."
	}
	return &d, nil
}

// isKnownActionType reports whether t is a workflow action the engine supports.
func isKnownActionType(t string) bool {
	switch t {
	case ActionReply, ActionReplyEphemeral, ActionCreateTask,
		ActionDeleteMessage, ActionWarnUser, ActionFlagToChannel:
		return true
	default:
		return false
	}
}

// draftSystemPrompt describes the workflow schema and the strict output rules.
func draftSystemPrompt() string {
	return strings.Join([]string{
		"You convert a plain-English description of a workspace automation into a single JSON object describing a workflow rule. Output ONLY the JSON object, no prose.",
		"",
		"A workflow is: WHEN a trigger fires, IF keywords match, DO actions.",
		"",
		"Schema:",
		"{",
		"  \"name\": short title for the rule,",
		"  \"trigger_type\": one of \"message_posted\" (a message is posted in a channel), \"user_joined_channel\" (someone joins a channel), \"meeting_ended\" (a call in a channel ends) or \"task_status_changed\" (a task moves to another status, e.g. into QA or Done),",
		"  \"trigger_config\": for task_status_changed only, {\"to_status\": the status the task moves into as the person named it, e.g. \"QA\" or \"done\"}; omit it to run on every move,",
		"  \"keywords\": array of trigger words (only for message_posted; [] for none),",
		"  \"match_type\": \"any\" or \"all\" (how keywords combine),",
		"  \"actions\": array of action objects,",
		"  \"notes\": optional one-line note about what the user must still fill in",
		"}",
		"",
		"Action objects (use the minimal set needed):",
		"- {\"type\":\"reply\",\"text\":\"...\"} post a public reply in the triggering channel (for task_status_changed, the channel the person picks). Supports {text},{channel},{user} placeholders; for task_status_changed use {task},{status},{from},{project},{by},{link}.",
		"- {\"type\":\"reply_ephemeral\",\"text\":\"...\"} private message to the triggering user.",
		"- {\"type\":\"create_task\",\"task_name\":\"...\",\"priority\":\"low|medium|high|urgent\",\"description\":\"...\"} create a task.",
		"- {\"type\":\"warn_user\",\"text\":\"...\"} private moderation warning to the author.",
		"- {\"type\":\"delete_message\"} remove the triggering message (message_posted only).",
		"- {\"type\":\"flag_to_channel\"} copy the message to a review channel (message_posted only).",
		"",
		"CRITICAL RULES:",
		"1. NEVER invent ids (channel ids, project ids, user ids). Do not include project_id, target_channel_id, or channel_id fields — the user selects those in the UI. If an action needs one, mention it in \"notes\".",
		"2. Choose the simplest trigger + actions that satisfy the request.",
		"3. delete_message and flag_to_channel are only valid with trigger_type \"message_posted\".",
		"4. Output a single valid JSON object and nothing else.",
	}, "\n")
}

// extractJSONObject returns the first top-level JSON object substring, tolerant
// of ```json fences and surrounding prose.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
