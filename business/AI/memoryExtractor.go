package business

// Workspace Memory extraction agent.
//
// Turns a body of workspace text (a meeting transcript/recap today; chat
// threads and docs later) into structured, deduplicated memory items —
// decisions, commitments, open questions — persisted in Postgres (system
// of record) and projected into OpenSearch (semantic recall). This is the
// structured complement to the raw-content vector index: it's what lets
// AskAI answer "what did we decide / who owns it / what's still open" with
// precision, and powers the "what does my workspace know" surface.
//
// Production properties:
//   - Opt-in via ai_settings.memory_layer_enabled.
//   - Bounded input (caller truncates) + circuit-breaker-guarded LLM call.
//   - Strict JSON contract with defensive parsing; malformed items are
//     skipped, never persisted.
//   - Idempotent: each item gets a stable dedup_hash so re-extraction of
//     the same conversation upserts rather than duplicates.
//   - Permission-scoped: items carry channel/project/grp scope; retrieval
//     filters by the caller's access (Postgres) and by the same rule in
//     the OpenSearch permission filter.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	// Cap the model's extraction output so a runaway response can't bloat
	// the DB. The prompt also asks for at most this many items.
	maxMemoryItemsPerRun = 25
	// Skip extraction entirely for trivially short inputs.
	minExtractChars = 200
)

// memoryExtractionPrompt instructs the model to emit a strict JSON array.
// Kept rigid for small local models: explicit schema, allowed enums, and
// an empty-array escape hatch so it never invents content.
const memoryExtractionPrompt = `You extract durable, structured knowledge from workspace text.
Return ONLY a JSON array (no prose, no markdown fences). Each element:
{
  "kind": "decision" | "commitment" | "question",
  "content": "<one self-contained sentence stating the fact>",
  "owner": "<person name if clearly stated, else empty>",
  "due": "<ISO date YYYY-MM-DD if a deadline is stated, else empty>",
  "confidence": <integer 0-100>
}

Rules:
- "decision": a concrete choice the group made.
- "commitment": something a specific person agreed to do.
- "question": an unresolved/open question raised but not answered.
- Use ONLY facts present in the text. Never invent owners, dates, or decisions.
- Each "content" must stand alone without the surrounding text.
- Return at most 25 items. If nothing durable is present, return exactly: []
- Output MUST be valid JSON. No trailing commas. No comments.`

// MemoryScope carries the permission scope + provenance for extracted
// items. Exactly one of ChannelUUID / ProjectUUID / ChatGrpID identifies
// where the source conversation lived.
type MemoryScope struct {
	ChannelUUID string
	ProjectUUID string
	ChatGrpID   string
	TeamUUID    string
	SourceType  string // memoryModels.Source*
	SourceUUID  string
	// CreatedByUUID is the app UUID of the user on whose behalf extraction
	// runs (e.g. a call participant). Used as created_by and as the
	// owner-visibility key when no explicit owner is resolved.
	CreatedByUUID string
}

// rawExtractedItem is the model's per-item JSON shape.
type rawExtractedItem struct {
	Kind       string `json:"kind"`
	Content    string `json:"content"`
	Owner      string `json:"owner"`
	Due        string `json:"due"`
	Confidence int    `json:"confidence"`
}

// MaybeExtractMemory runs extraction if the memory layer is enabled. Safe
// to call unconditionally; fire-and-forget friendly. Returns the count of
// items persisted (0 on no-op/disabled).
func MaybeExtractMemory(ctx context.Context, text string, scope MemoryScope) (int, error) {
	settings, err := getAISettingsForMemory(ctx)
	if err != nil {
		return 0, err
	}
	if !settings.enabled || !settings.memoryEnabled {
		return 0, nil
	}
	// Respect per-scope exclusions (trust/control): a channel/project/group
	// opted out of the memory layer is never extracted from.
	if scopeIsExcluded(ctx, scope) {
		return 0, nil
	}
	svc := ai.GetService()
	if !svc.IsEnabled() {
		return 0, nil
	}
	if len(strings.TrimSpace(text)) < minExtractChars {
		return 0, nil
	}

	// LLM extraction, circuit-breaker guarded.
	if err := svc.Resiliency.CB.Allow(); err != nil {
		return 0, fmt.Errorf("memory extract: circuit open: %w", err)
	}
	raw, err := svc.SummarizeFor(ctx, ai.PurposeMemory, text, memoryExtractionPrompt)
	if err != nil {
		svc.Resiliency.CB.RecordResult(err)
		return 0, fmt.Errorf("memory extract LLM: %w", err)
	}
	svc.Resiliency.CB.RecordSuccess()

	items := parseExtractedItems(raw)
	if len(items) == 0 {
		return 0, nil
	}

	persisted := 0
	for _, it := range items {
		if err := persistMemoryItem(ctx, it, scope); err != nil {
			// A tombstone means the user permanently deleted this exact fact;
			// honoring that is correct behavior, not an error. Skip quietly.
			if errors.Is(err, memoryModels.ErrTombstoned) {
				continue
			}
			helpers.LogErrorWithContext(ctx, "memory extract persist failed: %v", err)
			continue
		}
		persisted++
	}
	helpers.LogInfoWithContext(ctx, "memory extract: persisted %d/%d items (source=%s)", persisted, len(items), scope.SourceType)
	return persisted, nil
}

// parseExtractedItems defensively parses the model output into validated
// items. Tolerates code fences and leading/trailing prose by extracting
// the first JSON array. Invalid elements are skipped.
func parseExtractedItems(raw string) []rawExtractedItem {
	jsonArr := extractJSONArray(raw)
	if jsonArr == "" {
		return nil
	}
	var items []rawExtractedItem
	if err := json.Unmarshal([]byte(jsonArr), &items); err != nil {
		return nil
	}

	out := make([]rawExtractedItem, 0, len(items))
	for _, it := range items {
		kind := strings.ToLower(strings.TrimSpace(it.Kind))
		content := strings.TrimSpace(it.Content)
		if content == "" {
			continue
		}
		if kind != memoryModels.KindDecision &&
			kind != memoryModels.KindCommitment &&
			kind != memoryModels.KindQuestion {
			continue
		}
		// Bound content length defensively.
		if len(content) > 1000 {
			content = helpers.TruncateRunes(content, 1000)
		}
		it.Kind = kind
		it.Content = content
		if it.Confidence < 0 || it.Confidence > 100 {
			it.Confidence = 70
		}
		out = append(out, it)
		if len(out) >= maxMemoryItemsPerRun {
			break
		}
	}
	return out
}

// extractJSONArray returns the first top-level JSON array substring, or ""
// if none is found. Handles ```json fences and surrounding prose.
func extractJSONArray(s string) string {
	s = strings.TrimSpace(s)
	// Strip common code fences.
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start == -1 || end == -1 || end <= start {
		return ""
	}
	return s[start : end+1]
}

// persistMemoryItem upserts one item to Postgres and projects it to
// OpenSearch for semantic recall. Thin wrapper over the id-returning
// variant used by the extractor (which doesn't need the id). Automatic
// extraction never revives user tombstones (forceRevive=false).
func persistMemoryItem(ctx context.Context, it rawExtractedItem, scope MemoryScope) error {
	_, err := persistMemoryItemReturningID(ctx, it, scope, false)
	return err
}

// persistMemoryItemReturningID is the shared persist path for both the AI
// extractor and manual user capture: upsert to Postgres (system of record)
// + project to OpenSearch (semantic recall) + Dgraph (GraphRAG). Returns
// the persisted item id. forceRevive=true (manual capture) lets a deliberate
// re-save override a prior user-delete tombstone; the automatic extractor
// passes false so it honors tombstones.
func persistMemoryItemReturningID(ctx context.Context, it rawExtractedItem, scope MemoryScope, forceRevive bool) (uuid.UUID, error) {
	in := memoryModels.UpsertInput{
		Kind:        it.Kind,
		Content:     it.Content,
		Status:      memoryModels.StatusOpen,
		SourceType:  scope.SourceType,
		SourceUUID:  scope.SourceUUID,
		Confidence:  it.Confidence,
		ChatGrpID:   scope.ChatGrpID,
		DedupHash:   memoryDedupHash(it.Kind, it.Content, scope),
		ForceRevive: forceRevive,
	}

	// Scope UUIDs (parse best-effort; invalid → unscoped on that dimension).
	if scope.ChannelUUID != "" {
		if u, err := uuid.Parse(scope.ChannelUUID); err == nil {
			in.ChannelUUID = &u
		}
	}
	if scope.ProjectUUID != "" {
		if u, err := uuid.Parse(scope.ProjectUUID); err == nil {
			in.ProjectUUID = &u
		}
	}
	if scope.TeamUUID != "" {
		if u, err := uuid.Parse(scope.TeamUUID); err == nil {
			in.TeamUUID = &u
		}
	}
	if scope.CreatedByUUID != "" {
		if u, err := uuid.Parse(scope.CreatedByUUID); err == nil {
			in.CreatedBy = &u
			// Default owner to creator for commitments lacking a resolved owner.
			if it.Kind == memoryModels.KindCommitment {
				in.OwnerID = &u
			}
		}
	}

	// Parse a stated due date (YYYY-MM-DD) for commitments.
	if it.Due != "" {
		if t, err := time.Parse("2006-01-02", it.Due); err == nil {
			in.DueAt = &t
		}
	}

	id, err := memoryModels.Upsert(ctx, in)
	if err != nil {
		return uuid.Nil, err
	}

	// Project to OpenSearch for semantic recall. The embedded text is the
	// fact prefixed with its kind so vector matches read naturally.
	embedText := fmt.Sprintf("%s: %s", strings.Title(it.Kind), it.Content) //nolint:staticcheck // Title is fine for ASCII enum
	ai.EmbedMemoryContent(embedText, id.String(), scope.ChannelUUID, scope.ProjectUUID, scope.ChatGrpID, scope.CreatedByUUID)

	// Project to Dgraph for GraphRAG (linked owner/channel/project). Async,
	// best-effort: the graph is a queryable index, never the source of
	// truth, so a projection failure must not fail extraction.
	projectMemoryToGraphAsync(in, id.String())
	return id, nil
}

// memoryDedupHash produces a stable hash over (kind + normalized content +
// scope) so re-extraction of the same conversation upserts in place.
// Delegates to the model's canonical hasher so the dedup key has a single
// source of truth shared with the content-refresh-on-edit path.
//
// An instruction an agent was told to remember (RememberFact) also keys on its
// source and its author. An instruction is its author's: it is followed in the
// runs that act for them (AgentScopedMemoryBlock), so the same words from two
// people are two instructions, and neither is a fact the memory layer drew
// from the conversation. Keyed on the words alone, a second author's remember
// updated the first author's row, or revived it from a forget, and it stayed
// the first author's.
func memoryDedupHash(kind, content string, scope MemoryScope) string {
	if scope.SourceType == AgentMemorySourceType {
		content += "\x00" + scope.SourceType + "\x00" + strings.ToLower(strings.TrimSpace(scope.CreatedByUUID))
	}
	return memoryModels.DedupHash(kind, content, scope.ChannelUUID, scope.ProjectUUID, scope.ChatGrpID)
}

// aiMemorySettings is a tiny projection to avoid importing the adapter.
type aiMemorySettings struct {
	enabled       bool
	memoryEnabled bool
	nudgesEnabled bool
}

func getAISettingsForMemory(ctx context.Context) (aiMemorySettings, error) {
	s, err := getAISettingsRow(ctx)
	if err != nil {
		return aiMemorySettings{}, err
	}
	return aiMemorySettings{
		enabled:       s.Enabled,
		memoryEnabled: s.MemoryLayerEnabled,
		nudgesEnabled: s.NudgesEnabled,
	}, nil
}

// scopeIsExcluded reports whether the memory item's scope has been opted out
// of the memory layer. Checks the dimension the scope actually carries
// (channel / project / group). Fail-open on DB error (returns false) so a
// transient hiccup doesn't silently drop expected memory — the model logs.
func scopeIsExcluded(ctx context.Context, scope MemoryScope) bool {
	check := func(t, id string) bool {
		if id == "" {
			return false
		}
		ex, err := memoryModels.IsScopeExcluded(ctx, t, id)
		return err == nil && ex
	}
	return check(memoryModels.ExclusionChannel, scope.ChannelUUID) ||
		check(memoryModels.ExclusionProject, scope.ProjectUUID) ||
		check(memoryModels.ExclusionChatGrp, scope.ChatGrpID)
}

// getAISettingsRow fetches the singleton AI settings row.
func getAISettingsRow(ctx context.Context) (*aiModels.AISettings, error) {
	return aiModels.GetSettings(ctx)
}

// --- Retrieval side: injecting structured memory into AskAI context ---

// maxMemoryContextItems bounds how many memory items are injected into a
// single AskAI prompt. Small on purpose: this is high-signal structured
// data, and prompt tokens are the dominant latency cost on local models.
const maxMemoryContextItems = 8

// memoryIntentPatterns signal a question is about workspace STATE
// (decisions/commitments/status/open items) where structured memory adds
// value. For anything else (how-to, generic chat) memory is NOT injected,
// so it costs zero extra prompt tokens.
var memoryIntentPatterns = []string{
	"decid", "decision", "commit", "agree", "action item", "action items",
	"owe", "owner", "responsible", "assigned", "todo", "to-do", "to do",
	"deadline", "due", "open question", "open item", "still open", "what's open",
	"whats open", "left open", "unresolved", "pending", "follow up",
	"follow-up", "followup", "blocker", "blocked", "what's left", "whats left",
	"status", "next step", "next steps", "outstanding", "promised", "supposed to",
}

// isMemoryIntent reports whether structured memory should be injected for
// this question. Conservative: only true on a clear state/decision intent.
func isMemoryIntent(question string) bool {
	q := strings.ToLower(question)
	for _, p := range memoryIntentPatterns {
		if strings.Contains(q, p) {
			return true
		}
	}
	return false
}

// fetchMemoryItems returns recent OPEN memory items the user can see,
// bounded to maxMemoryContextItems. Permission-scoped by accessible
// channels/projects/DM-grouping-ids plus ownership. Best-effort.
func fetchMemoryItems(ctx context.Context, userInfo *userModels.UserInfo, channels, projects []string) []*memoryModels.MemoryItem {
	ownerID := userInfo.UserPostgresInfo.Id
	items, err := memoryModels.List(ctx, memoryModels.QueryFilter{
		Statuses:           []string{memoryModels.StatusOpen},
		AccessibleChannels: channels,
		AccessibleProjects: projects,
		AccessibleGrpIDs:   accessibleGroupingIDs(userInfo),
		OwnerID:            &ownerID,
		Limit:              maxMemoryContextItems,
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AI memory fetch failed: %v", err)
		return nil
	}
	return items
}

// accessibleGroupingIDs returns the DM/group-chat grouping ids the user is
// a participant in, so DM/group-scoped memory items are visible to ALL
// participants — not only the recap author (the owner fallback).
func accessibleGroupingIDs(userInfo *userModels.UserInfo) []string {
	var out []string
	seen := map[string]bool{}
	for _, dm := range userInfo.UserDgraphInfo.DMs {
		if dm.GroupingId != "" && !seen[dm.GroupingId] {
			seen[dm.GroupingId] = true
			out = append(out, dm.GroupingId)
		}
	}
	return out
}

// formatMemoryForLLM renders memory items as a compact, high-signal block.
// Returns "" when there are no items so nothing is added to the prompt.
func formatMemoryForLLM(items []*memoryModels.MemoryItem) string {
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Workspace Memory (structured — decisions, commitments, open questions)\n")
	for _, it := range items {
		label := strings.Title(it.Kind) //nolint:staticcheck // ASCII enum
		line := it.Content
		if len(line) > 240 {
			line = line[:240] + "…"
		}
		due := ""
		if it.DueAt != nil {
			due = " (due " + it.DueAt.Format("2006-01-02") + ")"
		}
		sb.WriteString(fmt.Sprintf("- [%s] %s%s\n", label, line, due))
	}
	sb.WriteString("\n")
	return sb.String()
}
