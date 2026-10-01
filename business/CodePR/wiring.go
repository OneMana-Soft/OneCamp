package codepr

// Wiring seams shared by BOTH triggers (an @mention and a task-assignment) and
// the tool executor: the durable-queue dedupe key (so duplicate/again-assigned
// work coalesces into one job instead of two conflicting PRs), the budget-input
// assembly (settings + agent caps + today's usage → the CheckBudget input), and
// the concise tool-result formatter (what the LLM / durable worker surfaces).
//
// All pure and unit-tested. The live registry/executor/trigger dispatch (which
// needs the real runner) supplies the concrete collaborators and calls these.

import (
	"fmt"
	"strings"
)

// TaskSourceType is the ai_agent_tasks source_type for a durable code-PR job, so
// it dedupes independently of other durable work.
const TaskSourceType = "code_pr"

// SourceID returns the STABLE durable-queue key for a coding task, so the same
// piece of work (a duplicate @mention on the same message, a re-assignment of
// the same task, a retry) coalesces onto one open job rather than opening two
// conflicting PRs (Req 1.9 / 8.8). It keys on the triggering surface entity
// (the post/comment/message/task that summoned the run), which is exactly what
// "the same work" means to a user. Falls back to a repo+instruction fingerprint
// when no surface id is available (e.g. an API/scheduled trigger), so even then
// two identical requests coalesce. Pure.
func SourceID(repo RepoRef, surface Surface, instruction string) string {
	switch surface.Kind {
	case SurfaceChannelPost:
		if id := strings.TrimSpace(surface.PostID); id != "" {
			return "post:" + id
		}
	case SurfaceDM, SurfaceGroupChat:
		if id := strings.TrimSpace(surface.MessageID); id != "" {
			return "msg:" + id
		}
	case SurfaceTask:
		if id := strings.TrimSpace(surface.TaskID); id != "" {
			return "task:" + id
		}
	}
	// No stable surface id: fingerprint repo + normalized instruction so two
	// identical requests still coalesce.
	fp := strings.ToLower(strings.Join(strings.Fields(instruction), " "))
	return "fp:" + repo.FullName() + ":" + fp
}

// BuildBudgetInput assembles the three-tier CheckBudget input from the admin
// config caps, the per-agent caps, and today's measured usage. A 0 cap is
// unlimited (CheckBudget honors that), so passing 0 for a tier that doesn't
// apply (e.g. no channel for a DM/task) correctly makes it never block. Pure.
func BuildBudgetInput(
	agentCapMinutes, agentCapRuns int, agentUsedMinutes, agentUsedRuns int,
	channelCapMinutes, channelCapRuns int, channelUsedMinutes, channelUsedRuns int,
	workspaceCapMinutes, workspaceCapRuns int, workspaceUsedMinutes, workspaceUsedRuns int,
) BudgetInput {
	return BudgetInput{
		AgentCap:       TierBudget{Minutes: agentCapMinutes, Runs: agentCapRuns},
		AgentUsage:     TierUsage{Minutes: agentUsedMinutes, Runs: agentUsedRuns},
		ChannelCap:     TierBudget{Minutes: channelCapMinutes, Runs: channelCapRuns},
		ChannelUsage:   TierUsage{Minutes: channelUsedMinutes, Runs: channelUsedRuns},
		WorkspaceCap:   TierBudget{Minutes: workspaceCapMinutes, Runs: workspaceCapRuns},
		WorkspaceUsage: TierUsage{Minutes: workspaceUsedMinutes, Runs: workspaceUsedRuns},
	}
}

// FormatToolResult renders an Outcome as the concise text the LLM tool-call
// result (and the durable worker's final surface) should carry. It is honest and
// bounded — a PR link on success, the question on a block, the honest reason
// otherwise — and never leaks internals (the Outcome messages are already
// sanitized). Pure.
func FormatToolResult(out Outcome) string {
	switch out.Status {
	case StatusOK:
		if strings.TrimSpace(out.Message) != "" {
			return out.Message
		}
		if out.PRURL != "" {
			return "Opened a pull request: " + out.PRURL
		}
		return "Done."
	case StatusBlocked:
		if q := strings.TrimSpace(out.NeedsHumanQuestion); q != "" {
			return q
		}
		if strings.TrimSpace(out.Message) != "" {
			return out.Message
		}
		return "I need more information to continue."
	default:
		msg := strings.TrimSpace(out.Message)
		if msg == "" {
			msg = "I couldn't complete the coding task."
		}
		return withFallbackLink(msg, out)
	}
}

// withFallbackLink guarantees a clickable link when a branch was pushed but no
// PR link is embedded in the message yet — so an errored/partial run is never a
// dead end. Prefers the one-click "open a PR" link, falls back to the branch.
func withFallbackLink(msg string, out Outcome) string {
	link := out.PRCreateURL
	if link == "" {
		link = out.BranchURL
	}
	if link == "" || strings.Contains(msg, link) {
		return msg
	}
	sep := " "
	if !strings.HasSuffix(msg, ".") {
		sep = ". "
	}
	return msg + sep + "You can open or review the change here: " + link
}

// IsAwaitingInput reports whether an outcome is a clean needs_human pause (parked
// as awaiting_input, resumable), as opposed to a hard failure. Pure.
func IsAwaitingInput(out Outcome) bool {
	return out.Status == StatusBlocked && strings.TrimSpace(out.NeedsHumanQuestion) != ""
}

// AuditFields renders a compact, log-safe one-line summary of an outcome for the
// run transcript / operator logs (no diff body, no token, no host paths). Pure.
func AuditFields(out Outcome) string {
	return fmt.Sprintf("status=%s repo=%s pr=%s head=%s files=%d verified=%v draft=%v in_scope=%v",
		out.Status, out.Repo.FullName(), out.PRURL, out.HeadBranch, out.DiffStat.Files,
		out.Verifier.AllPassed, out.Draft, out.Verdict.InScope)
}
