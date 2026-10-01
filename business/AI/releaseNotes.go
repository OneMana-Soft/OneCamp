package business

// AI release-notes drafter.
//
// Turns the pull requests merged in a recent window into a clean, user-facing
// release-notes / changelog draft. This is grounded in REAL shipped work (the
// merged PRs), so it is the high-value, non-hallucinated marketing artifact a
// software team actually needs and rewrites by hand today. The output is a
// DRAFT for a human to review/edit and publish — nothing is sent anywhere.
//
// Model-agnostic: drives the caller's resolved model via the standard chat
// path, bounded by the same token budget and resiliency as the assistant.

import (
	"context"
	"fmt"
	"strings"
	"time"

	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	releaseNotesMaxPRs     = 60
	releaseNotesMaxDays    = 180
	releaseNotesPerPRChars = 600 // cap each PR's body so one long PR can't dominate
)

const releaseNotesSystemPrompt = `You are a product writer drafting user-facing release notes from a list of merged pull requests.

Write concise, friendly release notes in markdown:
- Group changes under "### New", "### Improvements", and "### Fixes" (omit a group if empty). Infer the group from each PR's title/labels.
- One bullet per notable change, phrased for END USERS (what changed and why it matters), not for engineers. Merge trivial/duplicate PRs.
- Skip purely internal changes (chores, CI, refactors, dependency bumps) unless user-visible.
- Do not invent changes that are not in the list. Do not include PR numbers or author names in the bullets.
- Start directly with the groups. No preamble.`

// ReleaseNotesResult is a drafted changelog plus how it was scoped.
type ReleaseNotesResult struct {
	Notes   string `json:"notes"`    // markdown draft
	PRCount int    `json:"pr_count"` // merged PRs the draft is based on
	Days    int    `json:"days"`
}

// DraftReleaseNotes fetches PRs merged on owner/repo in the last `days` and
// drafts user-facing release notes for the calling user (using their resolved
// model). Rate-limited per user. Read-only against GitHub.
func DraftReleaseNotes(ctx context.Context, userUUID, owner, repo string, days int) (*ReleaseNotesResult, error) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, fmt.Errorf("AI service is not enabled")
	}
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("repo owner and name are required")
	}
	if days <= 0 {
		days = 14
	}
	if days > releaseNotesMaxDays {
		days = releaseNotesMaxDays
	}

	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if err := cb.Allow(); err != nil {
		return nil, err
	}
	if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		return nil, err
	}

	prs, err := githubBusiness.ListMergedPullRequests(ctx, owner, repo, days, releaseNotesMaxPRs)
	if err != nil {
		return nil, fmt.Errorf("fetch merged PRs: %w", err)
	}
	if len(prs) == 0 {
		return &ReleaseNotesResult{
			Notes:   fmt.Sprintf("No pull requests were merged in %s/%s in the last %d days.", owner, repo, days),
			PRCount: 0,
			Days:    days,
		}, nil
	}

	var b strings.Builder
	for _, pr := range prs {
		body := strings.TrimSpace(pr.Body)
		if len(body) > releaseNotesPerPRChars {
			body = body[:releaseNotesPerPRChars] + " ..."
		}
		labels := ""
		if len(pr.Labels) > 0 {
			labels = " [labels: " + strings.Join(pr.Labels, ", ") + "]"
		}
		b.WriteString(fmt.Sprintf("- %s%s\n%s\n\n", strings.TrimSpace(pr.Title), labels, body))
	}
	limits := ai.LimitsFrom(ctx)
	prList := limits.TruncateForPrompt(ctx, b.String(), limits.ContextBudget())

	messages := []ai.ChatMessage{
		{Role: "system", Content: releaseNotesSystemPrompt},
		{Role: "user", Content: fmt.Sprintf("Merged pull requests from the last %d days:\n\n%s\n\nDraft the release notes.", days, prList)},
	}
	opts := ai.ChatOptions{Temperature: 0.3, MaxTokens: 1200, Seed: int(time.Now().UnixNano() % 1000000)}

	answer, err := ai.ChatWithRescue(ctx, llm, messages, opts)
	if err != nil {
		cb.RecordResult(err)
		return nil, fmt.Errorf("release notes generation failed: %w", err)
	}
	cb.RecordSuccess()

	return &ReleaseNotesResult{
		Notes:   strings.TrimSpace(SanitizeResponse(answer)),
		PRCount: len(prs),
		Days:    days,
	}, nil
}
