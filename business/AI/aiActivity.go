package business

// Unified AI activity timeline — "what did the AI do, as whom, why".
//
// One read model that merges the AI's actions across the workspace into a
// single time-ordered feed for admins: autonomous agent runs (ai_agent_runs)
// and AI-attributable audit entries (web search, public-API/MCP tool calls,
// AI config changes). This is the enterprise trust + debugging surface — proof
// of every action the AI took, who it ran as, and the outcome — and the kind
// of governance view a self-hosted, AI-native workspace must have.
//
// Reuses existing authoritative stores (no parallel activity log): the agent
// run records and the tamper-evident admin audit chain. The merge/normalize/
// rank is a pure function so it is unit tested DB-free.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	botpost "github.com/akashc777/OneCamp/business/BotPost"
	agentDomain "github.com/akashc777/OneCamp/domain/AIAgent"
	"github.com/akashc777/OneCamp/helpers"
	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	"github.com/google/uuid"
)

const aiActivityMaxItems = 100

// aiActivityActionPrefixes are the audit actions this feed draws on.
//
// mcp. and agent. carry the refusals. A prefix list is used rather than naming
// every action because the set grows, and a feed that silently omits a new kind
// of decision is how this went wrong the first time.
var aiActivityActionPrefixes = []string{"ai.", "api.", "mcp.", "agent."}

// AIActivityActionPrefixes is the same list, for a caller assembling a proof
// over the same surface. Copied out so the caller cannot reorder or extend the
// live one, and shared rather than repeated so a proof can never cover a
// different set of actions from the feed it is a proof of.
func AIActivityActionPrefixes() []string {
	out := make([]string, len(aiActivityActionPrefixes))
	copy(out, aiActivityActionPrefixes)
	return out
}

// Activity statuses. Refused is not a failure: the system worked exactly as it
// was meant to, and a UI that paints it red beside real errors teaches people to
// treat the guarantee as a fault.
const (
	ActivityStatusRefused = "refused"
	ActivityStatusAllowed = "allowed"
)

// activityStatus reads the outcome out of an audit action.
//
// By suffix rather than a table of known actions: every decision this system
// records already ends in .refused or .allowed, and a lookup table is a list
// somebody has to remember to extend.
func activityStatus(action string) string {
	switch {
	case strings.HasSuffix(action, "."+ActivityStatusRefused):
		return ActivityStatusRefused
	case strings.HasSuffix(action, "."+ActivityStatusAllowed):
		return ActivityStatusAllowed
	default:
		return ""
	}
}

// AIActivityItem is one entry in the unified AI activity feed.
type AIActivityItem struct {
	Kind    string    `json:"kind"`             // agent_run | audit
	Title   string    `json:"title"`            // agent name / action label
	Actor   string    `json:"actor,omitempty"`  // who it ran as (email / "agent")
	Summary string    `json:"summary"`          // outcome / human description
	Status  string    `json:"status,omitempty"` // succeeded | failed | running | ""
	Source  string    `json:"source,omitempty"` // trigger source / category
	At      time.Time `json:"at"`               // when it happened (newest-first)

	// Optional routing for an agent_run (links to the run/agent in the builder).
	AgentID string `json:"agent_id,omitempty"`
	RunID   string `json:"run_id,omitempty"`

	// THE EVIDENCE, carried on audit rows so the feed and the log stop being two
	// accounts of one event.
	//
	// This feed is the readable version: an agent was stopped, here is why. The
	// audit log is the provable one. Without the sequence number and the hash
	// pair, a reader who wanted to get from one to the other had to search a log
	// by hand, and a member — who cannot open the admin log at all — had no way
	// to check the claim being made to them. The pair travels together because a
	// single hash demonstrates nothing: the link is what an edit breaks.
	Seq       int64  `json:"seq,omitempty"`
	PrevHash  string `json:"prev_hash,omitempty"`
	EntryHash string `json:"entry_hash,omitempty"`

	// WHO STARTED IT, as distinct from whose authority it carried. "schedule",
	// "event" and "handoff" ran with nobody watching; "person" had somebody in
	// the room. A member reading their own feed gets the same answer an auditor
	// gets from the log, because it is read from the same row.
	Initiator string `json:"initiator,omitempty"`
}

// AIActivity returns the unified, newest-first AI activity feed.
//
// Admins see the whole workspace. A member sees their own agents' runs and the
// decisions recorded against them as the actor — including refusals, which is the
// point: the person an agent acts for is entitled to know when it was stopped.
func AIActivity(ctx context.Context, isAdmin bool, ownerID *uuid.UUID, limit int) ([]AIActivityItem, error) {
	if limit <= 0 || limit > aiActivityMaxItems {
		limit = 50
	}

	// Agent runs: admins pass nil (whole workspace), members pass their id.
	scope := ownerID
	if isAdmin {
		scope = nil
	}
	runs, rerr := agentDomain.ListRecentRuns(ctx, scope, limit)
	if rerr != nil {
		helpers.LogErrorWithContext(ctx, "AIActivity: agent runs: %v", rerr)
		runs = nil
	}

	// AI-attributable audit entries: agent/tool/web-search/config actions.
	//
	// REFUSALS LIVE UNDER mcp. AND agent., and leaving them out meant the product
	// had no surface anywhere that showed one. The homepage sells "a denied call
	// leaves a row with the reason", the only refusal this system records is
	// mcp.tool_call.refused, and the feed filtered on "ai." and "api." — so that
	// row was written, hash-chained, and never shown to anybody.
	//
	// A MEMBER SEES THEIR OWN. The audit read used to be admin-only, so a member
	// got agent runs and no decisions at all: the person an agent acts FOR could
	// not see that it had been stopped on their behalf, and had to take the
	// guarantee on trust. The workspace-wide log stays admin-only — it carries
	// other people's actions and every config change — and a member's read is
	// scoped to themselves as the actor, which is their own record rather than a
	// wider grant.
	var scopeAudit *uuid.UUID
	if !isAdmin {
		if ownerID == nil {
			// No principal to scope to: show them nothing rather than everything.
			// Failing closed is the only safe direction for a log.
			return mergeAIActivity(runsToActivity(runs), nil, limit), nil
		}
		scopeAudit = ownerID
	}
	audits, aerr := auditModel.ListByActionPrefixes(ctx, aiActivityActionPrefixes, scopeAudit, limit)
	if aerr != nil {
		helpers.LogErrorWithContext(ctx, "AIActivity: audit: %v", aerr)
		audits = nil
	}

	return mergeAIActivity(runsToActivity(runs), auditsToActivity(audits), limit), nil
}

// runsToActivity maps agent run records into activity items. Pure.
func runsToActivity(runs []*agentModel.AgentRunActivity) []AIActivityItem {
	out := make([]AIActivityItem, 0, len(runs))
	for _, r := range runs {
		if r == nil {
			continue
		}
		out = append(out, AIActivityItem{
			Kind:    "agent_run",
			Title:   strings.TrimSpace(r.AgentName),
			Actor:   "agent",
			Summary: agentRunSummary(r),
			Status:  normalizeRunStatus(r.Status),
			Source:  strings.TrimSpace(r.TriggerSource),
			At:      r.StartedAt,
			AgentID: r.AgentId.String(),
			RunID:   r.Id.String(),
		})
	}
	return out
}

// agentRunSummary renders a concise outcome line for a run.
func agentRunSummary(r *agentModel.AgentRunActivity) string {
	if r.Error != nil && strings.TrimSpace(*r.Error) != "" {
		return "Failed: " + clipActivity(strings.TrimSpace(*r.Error), 140)
	}
	if r.Result != nil && strings.TrimSpace(*r.Result) != "" {
		// A chart in the result is named rather than clipped: the first 140
		// characters of its JSON tell a reader nothing.
		return clipActivity(strings.TrimSpace(botpost.ChartsAsText(*r.Result)), 140)
	}
	if r.StepCount > 0 {
		return fmt.Sprintf("%d step%s", r.StepCount, plural(r.StepCount))
	}
	return ""
}

// auditsToActivity maps audit entries into activity items. Pure.
func auditsToActivity(entries []*auditModel.AuditEntry) []AIActivityItem {
	out := make([]AIActivityItem, 0, len(entries))
	for _, e := range entries {
		if e == nil {
			continue
		}
		action := strings.TrimSpace(e.Action)
		out = append(out, AIActivityItem{
			Kind:    "audit",
			Title:   action,
			Actor:   strings.TrimSpace(e.ActorEmail),
			Summary: clipActivity(strings.TrimSpace(e.Summary), 200),
			Status:  activityStatus(action),
			Source:  strings.TrimSpace(e.Category),
			At:      e.CreatedAt,

			Seq:       e.Seq,
			PrevHash:  e.PrevHash,
			EntryHash: e.EntryHash,
			Initiator: metadataString(e.Metadata, auditBusiness.MetaInitiator),
		})
	}
	return out
}

// metadataString reads one string field out of an audit row's metadata JSON.
// Missing, malformed or non-string reads as "", which is the honest answer for
// a row written before the key existed or by a caller that did not say.
func metadataString(raw *string, key string) string {
	if raw == nil || *raw == "" {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(*raw), &m); err != nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

// mergeAIActivity merges the source lists, sorts newest-first, and caps the
// count. Stable so equal timestamps keep their source order. Pure + tested.
func mergeAIActivity(runs, audits []AIActivityItem, limit int) []AIActivityItem {
	merged := make([]AIActivityItem, 0, len(runs)+len(audits))
	merged = append(merged, runs...)
	merged = append(merged, audits...)
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].At.After(merged[j].At)
	})
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

// normalizeRunStatus maps stored run statuses onto a small display set.
func normalizeRunStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "succeeded", "success", "done", "completed":
		return "succeeded"
	case "failed", "error":
		return "failed"
	case "running", "in_progress", "pending":
		return "running"
	default:
		return strings.TrimSpace(s)
	}
}

// clipActivity trims s to at most max runes with an ellipsis. Pure.
func clipActivity(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
