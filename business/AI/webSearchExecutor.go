package business

// web_search tool executor. Read-only: runs the user's query against the
// admin-configured, provider-agnostic web search (services/AI/webSearch.go) and
// returns titled results with URLs + snippets for the model to ground on. The
// guarded HTTP client enforces residency (local-only blocks non-local
// endpoints); the call is rate-limited per user and audited content-free.

import (
	"context"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const webSearchToolMaxResults = 6

func executeWebSearch(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	if !ai.WebSearchEnabled() {
		return "", nil, fmt.Errorf("web search is not configured for this workspace")
	}
	query := strings.TrimSpace(action.Params["query"])
	if query == "" {
		return "", nil, fmt.Errorf("query is required")
	}

	// Per-user rate limit (a human/agent is waiting). The circuit breaker also
	// guards the search endpoint so a failing provider can't be hammered.
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return "", nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		return "", nil, err
	}
	if err := svc.Resiliency.CB.Allow(); err != nil {
		return "", nil, err
	}

	results, err := ai.WebSearch(ctx, query, webSearchToolMaxResults)
	if err != nil {
		svc.Resiliency.CB.RecordResult(err)
		helpers.LogInfoWithContext(ctx, "web_search failed: %v", err)
		return "", nil, fmt.Errorf("the web search could not be completed right now")
	}
	svc.Resiliency.CB.RecordSuccess()
	ai.AuditWebSearch(ctx, ai.WebSearchProviderName(), len(results))

	if len(results) == 0 {
		return fmt.Sprintf("No web results found for %q.", query), nil, nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Web results for %q:\n", query))
	for i, r := range results {
		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = r.URL
		}
		b.WriteString(fmt.Sprintf("%d. %s\n%s\n", i+1, title, r.URL))
		if s := strings.TrimSpace(r.Snippet); s != "" {
			b.WriteString(s + "\n")
		}
	}
	b.WriteString("\nCite the relevant links in your answer.")
	return strings.TrimSpace(b.String()), map[string]string{"tool": "web_search", "result_count": fmt.Sprintf("%d", len(results))}, nil
}
