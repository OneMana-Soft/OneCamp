package business

// Natural-language agent creation: describe the agent you want in plain English
// and the AI drafts a starting configuration (name, instructions, tools,
// trigger, autonomy) that prefills the builder for the human to review and
// tweak. It NEVER creates the agent itself — it returns a draft; the existing
// create flow (with all its validation + ownership) does the saving. Reuses the
// AI service chokepoint, so it is provider-agnostic and residency-respecting.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	draftMaxName         = 80
	draftMaxDescription  = 300
	draftMaxInstructions = 4000
	draftDefaultMaxSteps = 8
	draftMaxSteps        = 15
)

var draftValidTriggers = map[string]bool{"manual": true, "mention": true, "schedule": true, "event": true}
var draftValidAutonomy = map[string]bool{"auto": true, "approval": true, "plan": true}

// AgentDraft is a proposed agent configuration the builder prefills. It mirrors
// the subset of the create form the AI can sensibly fill; everything else keeps
// the form defaults.
type AgentDraft struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Instructions string   `json:"instructions"`
	EnabledTools []string `json:"enabled_tools"`
	TriggerType  string   `json:"trigger_type"`
	Autonomy     string   `json:"autonomy"`
	MaxSteps     int      `json:"max_steps"`
	DmAble       bool     `json:"dm_able"`
}

// DraftAgent asks the model to turn a natural-language goal into a starting
// agent configuration. Resilient (rate limit + per-user model + circuit
// breaker). Never persists anything.
func DraftAgent(ctx context.Context, actor Actor, prompt string) (*AgentDraft, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, fmt.Errorf("describe the agent you want to build")
	}
	if len(prompt) > 2000 {
		prompt = prompt[:2000]
	}

	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	key := actor.UserID.String()
	if err := svc.Resiliency.CheckRateLimit(ctx, key); err != nil {
		return nil, err
	}
	llm, cb := svc.ResolveUserModel(ctx, key)
	if llm == nil {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	if err := cb.Allow(); err != nil {
		return nil, err
	}

	out, err := ai.ChatJSONWithRetry(ctx, llm, cb, agentDraftSystemPrompt(), prompt,
		ai.ChatOptions{Temperature: 0.2, MaxTokens: 1200, LowLatency: true},
		func(s string) bool { _, e := parseAgentDraft(s); return e == nil })
	if err != nil {
		return nil, fmt.Errorf("the assistant could not draft an agent right now, please try again")
	}

	draft, perr := parseAgentDraft(out)
	if perr != nil {
		return nil, fmt.Errorf("the assistant returned an unexpected response, please try again")
	}
	return sanitizeAgentDraft(draft), nil
}

// draftToolCatalog returns the tool names + descriptions an agent may be granted
// (the static registry), so the model picks from real tools rather than
// inventing them.
func draftToolCatalog() ([]string, string) {
	var names []string
	var b strings.Builder
	for _, t := range ai.ToolRegistry {
		names = append(names, t.Name)
		b.WriteString("- " + t.Name + ": " + t.Description + "\n")
	}
	return names, b.String()
}

// draftValidToolSet is the set of grantable tool names (registry), for clamping
// the model's picks.
func draftValidToolSet() map[string]bool {
	set := make(map[string]bool)
	for _, t := range ai.ToolRegistry {
		set[t.Name] = true
	}
	return set
}

// agentDraftSystemPrompt instructs the model to emit a strict-JSON agent draft.
func agentDraftSystemPrompt() string {
	_, catalog := draftToolCatalog()
	return strings.Join([]string{
		"You design an AI agent for a team workspace from the user's description.",
		"Pick a concise name, a one-line description, clear operating instructions (what it does, its tone, and any boundaries), the MINIMAL set of tools it needs, a trigger, and an autonomy level.",
		"",
		"Available tools (choose ONLY from these exact names; pick the fewest that fit):",
		catalog,
		"Trigger types: \"manual\" (run by hand), \"mention\" (runs when @mentioned), \"schedule\" (recurring), \"event\" (on a workspace change). Default to \"manual\" unless the description clearly implies otherwise.",
		"Autonomy: \"auto\" (acts directly), \"approval\" (proposes writes for human approval), \"plan\" (plans then asks approval). Prefer \"approval\" when the agent performs writes (create/send/update); use \"auto\" for read-only or low-risk agents.",
		"",
		"Output rules (MUST follow exactly):",
		"- Output ONLY a single JSON object. No markdown, no code fences, no prose.",
		"- Schema: {\"name\": string, \"description\": string, \"instructions\": string, \"enabled_tools\": [string], \"trigger_type\": string, \"autonomy\": string, \"max_steps\": number, \"dm_able\": boolean}.",
		"- enabled_tools must be a subset of the exact tool names listed above (may be empty for a purely conversational agent).",
		"- Keep name under 80 chars, description one line, instructions a few sentences.",
	}, "\n")
}

// parseAgentDraft extracts the model's JSON object (tolerant of fences/prose).
func parseAgentDraft(raw string) (*AgentDraft, error) {
	js := extractDraftJSON(raw)
	if js == "" {
		return nil, fmt.Errorf("no JSON object found")
	}
	var d AgentDraft
	if err := json.Unmarshal([]byte(js), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// extractDraftJSON returns the first top-level JSON object substring.
func extractDraftJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || end <= start {
		return ""
	}
	return s[start : end+1]
}

// sanitizeAgentDraft clamps the model output to the allowed vocabulary + bounds
// so a sloppy/hostile draft can never produce an invalid form. Pure + tested.
func sanitizeAgentDraft(d *AgentDraft) *AgentDraft {
	out := &AgentDraft{}

	out.Name = clampDraft(d.Name, draftMaxName)
	if out.Name == "" {
		out.Name = "New agent"
	}
	out.Description = clampDraft(d.Description, draftMaxDescription)
	out.Instructions = clampDraft(d.Instructions, draftMaxInstructions)

	valid := draftValidToolSet()
	seen := map[string]bool{}
	for _, t := range d.EnabledTools {
		t = strings.TrimSpace(t)
		if valid[t] && !seen[t] {
			seen[t] = true
			out.EnabledTools = append(out.EnabledTools, t)
		}
	}

	out.TriggerType = strings.ToLower(strings.TrimSpace(d.TriggerType))
	if !draftValidTriggers[out.TriggerType] {
		out.TriggerType = "manual"
	}
	out.Autonomy = strings.ToLower(strings.TrimSpace(d.Autonomy))
	if !draftValidAutonomy[out.Autonomy] {
		out.Autonomy = "auto"
	}
	out.MaxSteps = d.MaxSteps
	if out.MaxSteps < 1 || out.MaxSteps > draftMaxSteps {
		out.MaxSteps = draftDefaultMaxSteps
	}
	out.DmAble = d.DmAble
	return out
}

// clampDraft trims whitespace, collapses internal newlines for short fields,
// and caps length.
func clampDraft(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = strings.TrimSpace(s[:max])
	}
	return s
}
