package business

// GraphRAG glue for the workspace-memory layer.
//
// Projects structured memory items into Dgraph as linked nodes (memory ↔
// owner/channel/project) and exposes a relationship read used to enrich
// AskAI answers about ownership/accountability. Projection is async + best-
// effort: Dgraph is a queryable INDEX over Postgres (the system of record),
// so a graph hiccup never blocks extraction or retrieval.

import (
	"context"
	"strings"
	"time"

	memDomain "github.com/akashc777/OneCamp/domain/WorkspaceMemory"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	memDgraph "github.com/akashc777/OneCamp/models/dgraph/WorkspaceMemory"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
)

// projectMemoryToGraphAsync mirrors a just-persisted memory item into
// Dgraph. Fire-and-forget with its own bounded context so it never couples
// extraction latency to graph availability.
func projectMemoryToGraphAsync(in memoryModels.UpsertInput, id string) {
	// Build the projection node from the same input we wrote to Postgres so
	// the two stores can't drift within a single persist.
	node := &dgraphStruct.DgraphMemoryItem{
		Uuid:       id,
		Kind:       in.Kind,
		Content:    in.Content,
		Status:     in.Status,
		Confidence: in.Confidence,
		DueAt:      in.DueAt,
		GrpID:      in.ChatGrpID,
	}
	now := time.Now()
	node.CreatedAt = &now
	node.UpdatedAt = &now

	var ownerUUID, channelUUID, projectUUID string
	if in.OwnerID != nil {
		ownerUUID = in.OwnerID.String()
	}
	if in.ChannelUUID != nil {
		channelUUID = in.ChannelUUID.String()
	}
	if in.ProjectUUID != nil {
		projectUUID = in.ProjectUUID.String()
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := memDgraph.UpsertMemoryItem(ctx, node, ownerUUID, channelUUID, projectUUID); err != nil {
			helpers.LogErrorWithContext(ctx, "memory graph projection failed for %s: %v", id, err)
		}
	}()
}

// updateGraphMemoryStatusAsync syncs a memory item's status into the Dgraph
// projection (best-effort, async) so graph "open items" reads reflect a
// resolve/dismiss action promptly.
func updateGraphMemoryStatusAsync(id, status string) {
	if id == "" || status == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := memDgraph.SetMemoryItemStatus(ctx, id, status); err != nil {
			helpers.LogErrorWithContext(ctx, "memory graph status sync failed for %s: %v", id, err)
		}
	}()
}

// formatGraphOwnedItemsForLLM renders graph-resolved owned open items into a
// compact block for the AskAI prompt. Returns "" when there are none.
// Distinct from formatMemoryForLLM (flat structured list): this is the
// OWNERSHIP view — "what the asker personally owns / has open" — surfaced
// when the question is about personal accountability.
func formatGraphOwnedItemsForLLM(items []*dgraphStruct.DgraphMemoryItem) string {
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Your Open Items (owned by you, from workspace memory)\n")
	for _, it := range items {
		label := strings.Title(it.Kind) //nolint:staticcheck // ASCII enum
		line := strings.TrimSpace(it.Content)
		if len(line) > 240 {
			line = line[:240] + "…"
		}
		where := ""
		if it.Channel != nil && it.Channel.Name != "" {
			where = " (in #" + it.Channel.Name + ")"
		} else if it.Project != nil && it.Project.Name != "" {
			where = " (in " + it.Project.Name + ")"
		}
		due := ""
		if it.DueAt != nil {
			due = " — due " + it.DueAt.Format("2006-01-02")
		}
		sb.WriteString("- [" + label + "] " + line + where + due + "\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

// fetchGraphOwnedItems returns the caller's own open memory items via the
// Dgraph owner edge. Best-effort: returns nil on any error so callers can
// treat it as "no extra context".
func fetchGraphOwnedItems(ctx context.Context, ownerUUID string, limit int) []*dgraphStruct.DgraphMemoryItem {
	if ownerUUID == "" {
		return nil
	}
	items, err := memDomain.QueryOwnedOpenItems(ctx, ownerUUID, "", limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "memory graph owned-items query failed: %v", err)
		return nil
	}
	return items
}

// fetchGraphScopeItems returns OPEN memory items linked to a channel or
// project via the reverse scope edge. Best-effort.
func fetchGraphScopeItems(ctx context.Context, scopeType, scopeUUID string, limit int) []*dgraphStruct.DgraphMemoryItem {
	if scopeType == "" || scopeUUID == "" {
		return nil
	}
	items, err := memDomain.QueryScopeOpenItems(ctx, scopeType, scopeUUID, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "memory graph scope-items query failed: %v", err)
		return nil
	}
	return items
}

// formatGraphScopeItemsForLLM renders a scope's open items grouped by kind
// with owner attribution, for the AskAI prompt. scopeLabel is the human
// name (e.g. "#design" or a project name). Returns "" when empty.
func formatGraphScopeItemsForLLM(scopeLabel string, items []*dgraphStruct.DgraphMemoryItem) string {
	if len(items) == 0 {
		return ""
	}
	var decisions, commitments, questions []*dgraphStruct.DgraphMemoryItem
	for _, it := range items {
		switch it.Kind {
		case memoryModels.KindDecision:
			decisions = append(decisions, it)
		case memoryModels.KindCommitment:
			commitments = append(commitments, it)
		case memoryModels.KindQuestion:
			questions = append(questions, it)
		default:
			questions = append(questions, it)
		}
	}

	var sb strings.Builder
	sb.WriteString("## Open Items in ")
	sb.WriteString(scopeLabel)
	sb.WriteString(" (from workspace memory)\n")
	writeGraphSection(&sb, "Decisions", decisions, false)
	writeGraphSection(&sb, "Commitments", commitments, true)
	writeGraphSection(&sb, "Open Questions", questions, true)
	sb.WriteString("\n")
	return sb.String()
}

// writeGraphSection renders one kind-section with optional owner attribution.
func writeGraphSection(sb *strings.Builder, heading string, items []*dgraphStruct.DgraphMemoryItem, showOwner bool) {
	if len(items) == 0 {
		return
	}
	sb.WriteString("### ")
	sb.WriteString(heading)
	sb.WriteString("\n")
	for _, it := range items {
		line := strings.TrimSpace(it.Content)
		if len(line) > 220 {
			line = line[:220] + "…"
		}
		owner := ""
		if showOwner && it.Owner != nil {
			name := it.Owner.UserName
			if name == "" {
				name = it.Owner.UserFullName
			}
			if name != "" {
				owner = " — @" + name
			}
		}
		due := ""
		if it.DueAt != nil {
			due = " (due " + it.DueAt.Format("2006-01-02") + ")"
		}
		sb.WriteString("- " + line + owner + due + "\n")
	}
}

// scopeRef is a minimal (uuid, name) pair for scope resolution, decoupled
// from the dgraph user struct so callers can build it from any source.
type scopeRef struct {
	UUID string
	Name string
}

// scopeRefsFromChannels / scopeRefsFromProjects adapt a UserInfo's
// accessible channels/projects into the minimal scopeRef list used by
// resolveScopeFromQuestion. Kept here so the resolver stays decoupled from
// the user model shape.
func scopeRefsFromChannels(userInfo *userModels.UserInfo) []scopeRef {
	out := make([]scopeRef, 0, len(userInfo.UserDgraphInfo.Channels))
	for _, ch := range userInfo.UserDgraphInfo.Channels {
		if ch.Uuid != "" && ch.Name != "" {
			out = append(out, scopeRef{UUID: ch.Uuid, Name: ch.Name})
		}
	}
	return out
}

func scopeRefsFromProjects(userInfo *userModels.UserInfo) []scopeRef {
	out := make([]scopeRef, 0, len(userInfo.UserDgraphInfo.Projects))
	for _, pr := range userInfo.UserDgraphInfo.Projects {
		if pr.Uuid != "" && pr.Name != "" {
			out = append(out, scopeRef{UUID: pr.Uuid, Name: pr.Name})
		}
	}
	return out
}

// resolveScopeFromQuestion inspects the question for a reference to one of
// the user's accessible channels or projects (by name or uuid) and returns
// the scope type/uuid/label to anchor a GraphRAG scope read. Returns empty
// strings when no scope is clearly referenced — conservative, so unrelated
// questions don't trigger a scope traversal. Prefers the longest matching
// name so "design-system" wins over "design".
func resolveScopeFromQuestion(question string, channels, projects []scopeRef) (scopeType, scopeUUID, scopeLabel string) {
	q := strings.ToLower(question)
	bestLen := 0
	for _, ch := range channels {
		if ch.Name == "" {
			continue
		}
		n := strings.ToLower(ch.Name)
		if (strings.Contains(q, n) || (ch.UUID != "" && strings.Contains(q, strings.ToLower(ch.UUID)))) && len(n) > bestLen {
			scopeType, scopeUUID, scopeLabel, bestLen = "channel", ch.UUID, "#"+ch.Name, len(n)
		}
	}
	if scopeType != "" {
		return scopeType, scopeUUID, scopeLabel
	}
	for _, pr := range projects {
		if pr.Name == "" {
			continue
		}
		n := strings.ToLower(pr.Name)
		if (strings.Contains(q, n) || (pr.UUID != "" && strings.Contains(q, strings.ToLower(pr.UUID)))) && len(n) > bestLen {
			scopeType, scopeUUID, scopeLabel, bestLen = "project", pr.UUID, pr.Name, len(n)
		}
	}
	return scopeType, scopeUUID, scopeLabel
}

// isOwnershipIntent reports whether a question is about personal
// accountability ("what do I own / what am I responsible for / my
// commitments"), where the graph ownership view adds value beyond the flat
// memory list. Conservative, mirrors isMemoryIntent's philosophy.
var ownershipIntentPatterns = []string{
	"my commitment", "my action item", "my action items", "what do i owe",
	"what am i responsible", "assigned to me", "i committed", "i promised",
	"my open", "my todo", "my to-do", "my tasks from", "what i owe",
	"what's on my plate", "whats on my plate", "on my plate",
	"my responsibilities", "what i committed", "what i promised",
}

func isOwnershipIntent(question string) bool {
	q := strings.ToLower(question)
	for _, p := range ownershipIntentPatterns {
		if strings.Contains(q, p) {
			return true
		}
	}
	return false
}
