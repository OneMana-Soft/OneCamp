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
	releaseBusiness "github.com/akashc777/OneCamp/business/Release"
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
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("repo owner and name are required")
	}
	// Before the AI is asked: a repository the asker may not read is refused
	// whatever else holds.
	if err := CodeRepoAllowed(ctx, userUUID, owner, repo); err != nil {
		return nil, err
	}
	if days <= 0 {
		days = 14
	}
	if days > releaseNotesMaxDays {
		days = releaseNotesMaxDays
	}
	var prs []githubBusiness.MergedPR
	notes, err := askerDraft(ctx, userUUID, func() ([]ai.ChatMessage, error) {
		var err error
		if prs, err = githubBusiness.ListMergedPullRequests(ctx, owner, repo, days, releaseNotesMaxPRs); err != nil || len(prs) == 0 {
			return nil, err
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
		return []ai.ChatMessage{
			{Role: "system", Content: releaseNotesSystemPrompt},
			{Role: "user", Content: fmt.Sprintf("Merged pull requests from the last %d days:\n\n%s\n\nDraft the release notes.", days, prList)},
		}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("release notes: %w", err)
	}
	if len(prs) == 0 {
		notes = fmt.Sprintf("No pull requests were merged in %s/%s in the last %d days.", owner, repo, days)
	}
	return &ReleaseNotesResult{Notes: notes, PRCount: len(prs), Days: days}, nil
}

// askerDraft asks userUUID's own model for a draft after the checks every
// draft passes, in turn: the AI on, the model's breaker closed, the asker
// under their rate limit. prompt runs only once they pass, so a refused draft
// reads nothing; nil messages is nothing to draft from, answered "" without
// asking. DraftReleaseNotes and a release's notes (draftNotesFromTasks) share it.
func askerDraft(ctx context.Context, userUUID string, prompt func() ([]ai.ChatMessage, error)) (string, error) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return "", fmt.Errorf("AI service is not enabled")
	}
	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if err := cb.Allow(); err != nil {
		return "", err
	}
	if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		return "", err
	}
	messages, err := prompt()
	if err != nil || messages == nil {
		return "", err
	}
	opts := ai.ChatOptions{Temperature: 0.3, MaxTokens: 1200, Seed: int(time.Now().UnixNano() % 1000000)}
	answer, err := ai.ChatWithRescue(ctx, llm, messages, opts)
	if err != nil {
		cb.RecordResult(err)
		return "", fmt.Errorf("generation failed: %w", err)
	}
	cb.RecordSuccess()
	return strings.TrimSpace(SanitizeResponse(answer)), nil
}

// The AI edition drafts a release's notes (business/Release's slot).
func init() { releaseBusiness.RegisterNotesDrafter(draftNotesFromTasks) }

const releaseTasksPrompt = `You are a product writer drafting user-facing release notes from the tasks a release ships, each with the titles of the pull requests that made it.

Write concise, friendly release notes in markdown:
- Group changes under "### New", "### Improvements", and "### Fixes" (omit a group if empty). Infer the group from each task and its pull requests.
- One bullet per notable change, phrased for END USERS (what changed and why it matters). Merge duplicates.
- Skip purely internal work unless user-visible. Tasks not done may be left out or named as coming.
- Do not invent changes that are not in the list. Do not include task keys, PR numbers or names in the bullets.
- Start directly with the groups. No preamble.`

// draftNotesFromTasks drafts a release's notes from its brief: the tasks its
// asker may read, with their pull requests' titles (business/Release built
// it). A draft refused or failed is answered in the asker's words.
func draftNotesFromTasks(ctx context.Context, asker string, b releaseBusiness.NotesBrief) (string, error) {
	notes, err := askerDraft(ctx, asker, func() ([]ai.ChatMessage, error) {
		if len(b.Tasks) == 0 {
			return nil, nil
		}
		var list strings.Builder
		for _, t := range b.Tasks {
			fmt.Fprintf(&list, "- %s %s [%s]\n", t.Key, t.Name, t.Status)
			for _, pr := range t.PRs {
				fmt.Fprintf(&list, "  pull request: %s\n", pr)
			}
		}
		limits := ai.LimitsFrom(ctx)
		return []ai.ChatMessage{
			{Role: "system", Content: releaseTasksPrompt},
			{Role: "user", Content: fmt.Sprintf("Release %s ships these tasks:\n\n%s\nDraft the release notes.", b.Release,
				limits.TruncateForPrompt(ctx, list.String(), limits.ContextBudget()))},
		}, nil
	})
	if err != nil {
		code, words := ai.ClientError(ctx, err)
		return "", &releaseBusiness.DraftRefusal{Code: code, Msg: words}
	}
	if len(b.Tasks) == 0 {
		return "This release has no tasks you can see to draft notes from yet.", nil
	}
	return notes, nil
}
