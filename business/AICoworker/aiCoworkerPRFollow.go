package aicoworker

// PR follow-up: when someone comments on — or requests changes in a review of —
// a pull request an AI teammate opened, propose the change they ask for.
//
// The code-PR job that opened the PR has already reached a terminal state, and
// a GitHub webhook only knows the PR URL. Migration 128 persists the OneCamp
// reply surface on code_pr_runs, so this listener maps the PR URL back to the
// agent and the thread it posted to. A GitHub commenter is nobody in OneCamp
// (on a public repository, anyone), so the feedback is proposed to the owner of
// the account the job pushes with (agentbusiness.ProposePullRequestFeedback),
// and only once they approve does a coding follow-up address it, posting back
// to that thread.
//
// Decoupled via the workspace event bus (the GitHub package stays AI-free), and
// bot senders are dropped upstream so an app/agent comment can't self-drive.

import (
	"context"
	"encoding/json"
	"strings"

	agentbusiness "github.com/akashc777/OneCamp/business/AIAgent"
	codepr "github.com/akashc777/OneCamp/business/CodePR"
	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// handlePRComment routes a PR comment / actionable review back to the agent that
// opened the PR. Consumes github.pr.comment (a PR conversation comment) and
// github.pr.review_submitted (a review — continued only when it requests changes
// or carries a body; an empty approval needn't wake the agent). No-op when AI is
// off, when no agent opened the PR, or when the run had no in-thread surface.
func handlePRComment(ctx context.Context, eventType string, data map[string]interface{}) {
	if svc := ai.GetService(); svc == nil || !svc.IsEnabled() {
		return
	}
	prURL, _ := data["pr_url"].(string)
	body, _ := data["body"].(string)
	commenter, _ := data["commenter_login"].(string)
	if strings.TrimSpace(commenter) == "" {
		commenter, _ = data["reviewer"].(string)
	}
	prURL = strings.TrimSpace(prURL)
	body = strings.TrimSpace(body)
	if prURL == "" {
		return
	}

	// A review only wakes the agent on actionable feedback: changes requested,
	// or a review that carries a body. A bare approve/comment is not a follow-up.
	if eventType == "github.pr.review_submitted" {
		state, _ := data["review_state"].(string)
		if !strings.EqualFold(strings.TrimSpace(state), "CHANGES_REQUESTED") && body == "" {
			return
		}
	}
	if body == "" {
		return
	}

	agentID, surfaceRaw, found, err := aiModels.GetCodePRRunSurfaceByPRURL(ctx, prURL)
	if err != nil || !found || agentID == nil {
		return // no agent opened this PR (a human PR) — cheap best-effort no-op
	}
	entityID := codePRSurfaceEntityID(surfaceRaw)
	if entityID == "" {
		return // the run had no in-thread surface to continue in
	}

	// Only to the agent that opened the pull request, and as a proposal to the
	// owner of the account it pushes with: never into the thread's work at
	// large, where a commenter nobody can identify would steer whatever runs.
	agentbusiness.ProposePullRequestFeedback(ctx, *agentID, entityID, synthPRCommentFollowup(prURL, commenter, body))
}

// codePRSurfaceEntityID extracts the OneCamp thread entity id (post / message /
// task uuid) from a stored code-PR reply surface (codepr.Surface JSON), or ""
// when there is none to continue in (assistant/API trigger, or an older row).
func codePRSurfaceEntityID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var s codepr.Surface
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return ""
	}
	switch {
	case strings.TrimSpace(s.PostID) != "":
		return strings.TrimSpace(s.PostID)
	case strings.TrimSpace(s.MessageID) != "":
		return strings.TrimSpace(s.MessageID)
	case strings.TrimSpace(s.TaskID) != "":
		return strings.TrimSpace(s.TaskID)
	default:
		return ""
	}
}

// synthPRCommentFollowup frames PR feedback as a one-line follow-up for the
// continuation engine (which wraps it with the generic "continue your work"
// instruction). Keeps the PR URL + author so the agent has full context.
func synthPRCommentFollowup(prURL, commenter, body string) string {
	who := strings.TrimSpace(commenter)
	if who == "" {
		who = "A reviewer"
	}
	return who + " left feedback on the pull request you opened (" + prURL + "): " +
		strings.TrimSpace(helpers.HTMLToPlainText(body))
}
