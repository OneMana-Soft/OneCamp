package business

// Turning a finished Slack import into an answer instead of a receipt.
//
// The import already ends with a number. A number proves the machine worked; it
// says nothing about whether the years of conversation now sitting in the
// workspace are worth opening. This is the one moment where the product can
// describe the customer's OWN data back to them, on their own server, and it was
// being spent on "12,400 messages imported".
//
// It reuses the catch-up machinery wholesale — the same permission-scoped
// retrieval, the same resiliency gate, the same truncation and sanitisation —
// because this is the same question asked over a different window: not "what did
// I miss since Tuesday" but "what is in what I just brought over". A second
// retrieval path would be a second thing to keep correct on the side of the
// boundary that most needs to stay right.

import (
	"context"
	"fmt"
	"strings"

	importBusiness "github.com/akashc777/OneCamp/business/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	// importDigestMaxItems caps how much imported content feeds the summary.
	// Higher than catch-up's window because an import is a one-off over years of
	// history rather than a per-open recap, so a wider read is worth paying for
	// once. The token ceiling below is still the real bound.
	importDigestMaxItems = 200

	// importDigestSince is the floor for the retrieval window.
	//
	// Zero, not a lookback. Imported messages are backdated to their original
	// Slack timestamps, so any recency floor would silently exclude exactly the
	// history the customer just paid to move. FetchUnreadAcrossScopes caps to the
	// NEWEST items within the window, so an unbounded floor still yields the most
	// recent conversation rather than the oldest.
	importDigestSince = 0
)

// importDigestSystemPrompt tunes the model for "what did I just bring over".
//
// It is told to describe and not to welcome, because the failure mode of a
// summary shown at the end of a migration is enthusiastic filler about how
// exciting the workspace looks. It is also told to say so when the sample is
// thin, so a five-message import produces a short honest line rather than a
// confident account of a team that barely spoke.
const importDigestSystemPrompt = `You are summarising a Slack workspace that has just been imported into OneCamp.

Write a short brief for the person who ran the import, describing what is now in their workspace.

Rules:
- Lead with the substance: what were these people actually working on and talking about.
- Name recurring themes, decisions that appear settled, and threads that look unresolved.
- Use the participants' own vocabulary for their projects and systems.
- 120 words or fewer, plain prose, no headings, no bullet lists, no preamble.
- Do not welcome the user, do not describe OneCamp, and do not mention the import process.
- If the sample is too thin to characterise, say that plainly in one sentence instead of guessing.`

// generateImportDigest is the Digester installed into the import package.
//
// Every early return is a deliberate silent skip. The import has already been
// reported as completed by the time this runs, so an unconfigured provider, an
// open circuit or an empty index must produce no digest rather than an error the
// customer would read as a failed migration.
func generateImportDigest(ctx context.Context, req importBusiness.DigestRequest) (string, error) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return "", nil // AI configured off. Not an error, just nothing to say.
	}
	user := req.ImportingUser
	if user == nil {
		return "", nil
	}

	// Never read wider than the importer could read themselves. The import knows
	// which channels it created; this intersects that with what this user may
	// actually see, so a digest cannot describe a private channel they were
	// mapped into but are not a member of.
	visible := visibleImportedChannels(user, req.ChannelUUIDs)
	if len(visible) == 0 {
		return "", nil
	}

	userUUID := user.UserDgraphInfo.Uuid
	items, err := ai.FetchUnreadAcrossScopes(ctx, userUUID, visible, nil, nil, importDigestSince, importDigestMaxItems)
	if err != nil {
		return "", fmt.Errorf("import digest fetch failed: %w", err)
	}
	if len(items) == 0 {
		return "", nil // Indexed nothing worth reading. No LLM call.
	}

	// Only now, with real content in hand, spend a call against the shared rate
	// limiter and circuit breaker.
	if err := svc.Resiliency.PreCheck(ctx, userUUID); err != nil {
		return "", err
	}

	content := formatContentWindow(items)
	limits := ai.LimitsFrom(ctx)
	content = limits.TruncateForPrompt(ctx, content, limits.ContextBudget())

	summary, err := svc.Summarize(ctx, content, importDigestSystemPrompt)
	if err != nil {
		svc.Resiliency.CB.RecordResult(err)
		return "", fmt.Errorf("import digest summarization failed: %w", err)
	}
	svc.Resiliency.CB.RecordSuccess()

	return strings.TrimSpace(SanitizeResponse(summary)), nil
}

// visibleImportedChannels intersects the channels an import touched with the
// channels this user may read.
//
// Order follows the imported list rather than the accessible one so the result
// is stable across calls for the same job, which keeps a regenerated digest
// reading over the same window.
func visibleImportedChannels(user *userModels.UserInfo, imported []string) []string {
	accessible, _ := getAccessibleResourceUUIDs(user)
	out := make([]string, 0, len(imported))
	for _, ch := range imported {
		if containsStr(accessible, ch) {
			out = append(out, ch)
		}
	}
	return out
}

// init installs the digester. Linking this package is what gives the import a
// digest; the AI-free edition does not link it and the import silently has none.
func init() {
	importBusiness.RegisterImportDigester(generateImportDigest)
}
