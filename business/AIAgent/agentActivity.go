package business

// Agent activity — the end-user-facing "show your work" timeline. Every
// platform converged on the same 2026 lesson: autonomous agents must show what
// they did. This surfaces the EXISTING ai_agent_runs record (the authoritative
// per-run transcript + outcome) as a render-ready feed, scoped to what the
// caller may see (admins: whole workspace; members: agents they own). No new
// store: it reads runs joined to agents, and derives "which tools it used /
// what it produced" purely from the persisted transcript.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	botpost "github.com/akashc777/OneCamp/business/BotPost"
	agentDomain "github.com/akashc777/OneCamp/domain/AIAgent"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

const (
	activityMaxItems      = 50
	activitySummaryMaxLen = 220
)

// AgentActivityItem is one entry in the agent activity feed: what an agent did,
// when, and how it turned out. Render-ready; no raw transcript.
type AgentActivityItem struct {
	RunID          string   `json:"run_id"`
	AgentID        string   `json:"agent_id"`
	AgentName      string   `json:"agent_name"`
	AgentAvatarKey string   `json:"agent_avatar_key,omitempty"`
	Status         string   `json:"status"`         // succeeded | failed | running | stopped
	TriggerSource  string   `json:"trigger_source"` // manual | mention | schedule | event:* | test
	ToolsUsed      []string `json:"tools_used"`     // distinct tools the run invoked, in first-seen order
	ActionCount    int      `json:"action_count"`   // tool calls that actually executed
	Summary        string   `json:"summary"`        // short, human "what it did"
	Error          string   `json:"error,omitempty"`
	Steps          int      `json:"steps"`
	Tokens         int64    `json:"tokens"`
	StartedAt      string   `json:"started_at"`
	EndedAt        string   `json:"ended_at,omitempty"`
}

// RecentActivity returns the most recent agent runs the actor may see, newest
// first. Admins see the whole workspace; members see only agents they own
// (same scoping as the rest of the builder).
func RecentActivity(ctx context.Context, actor Actor, limit int) ([]AgentActivityItem, error) {
	if limit <= 0 || limit > activityMaxItems {
		limit = activityMaxItems
	}
	var createdBy *uuid.UUID
	if !actor.IsAdmin {
		id := actor.UserID
		createdBy = &id
	}
	runs, err := agentDomain.ListRecentRuns(ctx, createdBy, limit)
	if err != nil {
		return nil, err
	}
	out := make([]AgentActivityItem, 0, len(runs))
	for _, r := range runs {
		out = append(out, toActivityItem(r))
	}
	return out, nil
}

// toActivityItem maps a joined run row into the render-ready feed item.
func toActivityItem(r *model.AgentRunActivity) AgentActivityItem {
	tools, actionCount := parseRunTranscript(r.Steps)
	item := AgentActivityItem{
		RunID:         r.Id.String(),
		AgentID:       r.AgentId.String(),
		AgentName:     strings.TrimSpace(r.AgentName),
		Status:        r.Status,
		TriggerSource: r.TriggerSource,
		ToolsUsed:     tools,
		ActionCount:   actionCount,
		Summary:       runSummary(r, tools),
		Steps:         r.StepCount,
		Tokens:        r.Tokens,
		StartedAt:     r.StartedAt.Format(time.RFC3339),
	}
	if r.AgentAvatarKey != nil {
		item.AgentAvatarKey = *r.AgentAvatarKey
	}
	if r.Error != nil {
		item.Error = trimOneLineActivity(*r.Error, activitySummaryMaxLen)
	}
	if r.EndedAt != nil {
		item.EndedAt = r.EndedAt.Format(time.RFC3339)
	}
	return item
}

// transcriptStep mirrors the persisted run transcript shape (agentRunner's
// stepRecord/toolCallRecord) for read-only parsing. Kept local + minimal so a
// transcript-format change is a single edit here.
type transcriptStep struct {
	ToolCalls []struct {
		Tool    string `json:"tool"`
		Error   string `json:"error,omitempty"`
		Skipped string `json:"skipped,omitempty"`
		Remote  bool   `json:"remote,omitempty"`
	} `json:"tool_calls,omitempty"`
}

// parseRunTranscript derives the distinct tools a run invoked (first-seen
// order) and how many tool calls actually executed (not skipped, no error),
// from the persisted JSON transcript. Pure + unit-tested. A malformed/empty
// transcript yields no tools, never an error.
func parseRunTranscript(stepsJSON string) (tools []string, actionCount int) {
	s := strings.TrimSpace(stepsJSON)
	if s == "" || s == "null" {
		return nil, 0
	}
	var steps []transcriptStep
	if err := json.Unmarshal([]byte(s), &steps); err != nil {
		return nil, 0
	}
	seen := map[string]bool{}
	for _, st := range steps {
		for _, tc := range st.ToolCalls {
			name := strings.TrimSpace(tc.Tool)
			if name == "" {
				continue
			}
			if !seen[name] {
				seen[name] = true
				tools = append(tools, name)
			}
			// A remote brain's own work is named among the tools, because it
			// happened, and left out of the count of actions this workspace
			// took, because it was not one.
			if tc.Skipped == "" && tc.Error == "" && !tc.Remote {
				actionCount++
			}
		}
	}
	return tools, actionCount
}

// runSummary builds a short, human "what it did" line: prefer the run's final
// result text; otherwise describe the tools it used; otherwise fall back to the
// status. Pure.
func runSummary(r *model.AgentRunActivity, tools []string) string {
	if r.Result != nil {
		// A chart in the result is named rather than flattened to one line of
		// its JSON.
		if s := trimOneLineActivity(botpost.ChartsAsText(*r.Result), activitySummaryMaxLen); s != "" {
			return s
		}
	}
	if len(tools) > 0 {
		shown := tools
		if len(shown) > 4 {
			shown = shown[:4]
		}
		return "Used " + strings.Join(humanizeTools(shown), ", ")
	}
	switch r.Status {
	case model.RunFailed:
		return "Run failed"
	case model.RunRunning:
		return "Running…"
	case model.RunStopped:
		return "Stopped"
	default:
		return "Completed with no changes"
	}
}

// humanizeTools turns tool ids (create_task) into readable labels (create task),
// preserving order.
func humanizeTools(tools []string) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, strings.ReplaceAll(t, "_", " "))
	}
	return out
}

// trimOneLineActivity collapses whitespace/newlines and caps length.
func trimOneLineActivity(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = strings.TrimSpace(s[:max]) + "…"
	}
	return s
}
