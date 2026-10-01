package business

// Agent conversational memory — the "remember / forget" capability that lets a
// teammate give an AI agent standing instructions in a channel or DM ("remember
// for this channel: keep replies short, always link the source"), the way you'd
// tell a colleague. It is the OneCamp analog of Claude Tag's channel memory.
//
// Storage reuses the governed Workspace Memory store (kind=glossary,
// source_type=agent_memory) rather than a parallel table, so a remembered fact
// is permission-scoped, tombstone-aware, admin-reviewable in the memory panel,
// and de-duplicated — for free. These are EXPLICIT agent instructions (not the
// passive transcript extractor), so they are intentionally independent of the
// passive memory-layer toggle; scope + tombstones still govern them.

import (
	"context"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// AgentMemorySourceType marks a workspace_memory_items row as an agent-remembered
// standing instruction (vs a transcript-extracted or user-captured fact), so
// remember/forget only ever touch their own rows and never a user's memory.
const AgentMemorySourceType = "agent_memory"

// maxAgentMemoryInject bounds how many remembered facts are injected into a run's
// system prompt — small, since prompt tokens dominate latency on local models.
const maxAgentMemoryInject = 12

// RememberFact persists a durable, scope-bound standing fact/instruction an agent
// was told to remember (kind=glossary, source=agent_memory), scoped to the
// channel OR chat grouping the run happened in. forceRevive=true: an agent
// re-remembering a fact a user once deleted is an explicit, intentional
// re-capture. Returns the persisted item id. content is trimmed + length-capped.
func RememberFact(ctx context.Context, channelUUID, chatGrpID, ownerUUID, content string) (uuid.UUID, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return uuid.Nil, fmt.Errorf("nothing to remember")
	}
	if len(content) > 1000 {
		content = helpers.TruncateRunes(content, 1000)
	}
	if strings.TrimSpace(channelUUID) == "" && strings.TrimSpace(chatGrpID) == "" {
		return uuid.Nil, fmt.Errorf("no channel or conversation to remember this for")
	}
	scope := MemoryScope{
		ChannelUUID:   strings.TrimSpace(channelUUID),
		ChatGrpID:     strings.TrimSpace(chatGrpID),
		CreatedByUUID: strings.TrimSpace(ownerUUID),
		SourceType:    AgentMemorySourceType,
	}
	item := rawExtractedItem{Kind: memoryModels.KindGlossary, Content: content, Confidence: 100}
	return persistMemoryItemReturningID(ctx, item, scope, true)
}

// ForgetFacts soft-deletes the agent-remembered facts in a scope whose content
// contains query (case-insensitive); an empty query forgets ALL of them for the
// scope. It only ever removes agent_memory-sourced glossary rows in the exact
// scope, so it can never delete a user-captured or transcript-extracted memory.
// Returns the number removed.
func ForgetFacts(ctx context.Context, channelUUID, chatGrpID, query string) (int, error) {
	items := listAgentMemory(ctx, channelUUID, chatGrpID)
	if len(items) == 0 {
		return 0, nil
	}
	q := strings.ToLower(strings.TrimSpace(query))
	removed := 0
	for _, it := range items {
		if q != "" && !strings.Contains(strings.ToLower(it.Content), q) {
			continue
		}
		if derr := memoryModels.SoftDelete(ctx, it.ID); derr == nil {
			removed++
			// "Forget" must mean forget everywhere: drop the semantic (OpenSearch)
			// + graph (Dgraph) projections too, so a forgotten instruction can't
			// resurface via AskAI recall. Best-effort, async — mirrors the memory
			// dismiss path.
			ai.DeleteMemoryProjectionsAsync(it.ID.String())
		}
	}
	return removed, nil
}

// AgentScopedMemoryBlock renders the agent-remembered standing instructions for a
// channel/group as a compact block for injection into a run's system prompt, or
// "" when there are none. This is what makes a remembered instruction actually
// take effect on future runs in that conversation.
func AgentScopedMemoryBlock(ctx context.Context, channelUUID, chatGrpID string) string {
	items := listAgentMemory(ctx, channelUUID, chatGrpID)
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nStanding instructions you were asked to remember for this conversation (follow them unless a newer instruction overrides one):\n")
	for _, it := range items {
		line := strings.TrimSpace(it.Content)
		if line == "" {
			continue
		}
		if len(line) > 240 {
			line = line[:240] + "…"
		}
		b.WriteString("- " + line + "\n")
	}
	return b.String()
}

// listAgentMemory returns the OPEN agent-remembered glossary items scoped to the
// given channel/group, newest first, capped. Filters to the agent_memory source
// so only remembered instructions are returned (not user/extracted memory).
func listAgentMemory(ctx context.Context, channelUUID, chatGrpID string) []*memoryModels.MemoryItem {
	channelUUID = strings.TrimSpace(channelUUID)
	chatGrpID = strings.TrimSpace(chatGrpID)
	if channelUUID == "" && chatGrpID == "" {
		return nil
	}
	f := memoryModels.QueryFilter{
		Kinds:    []string{memoryModels.KindGlossary},
		Statuses: []string{memoryModels.StatusOpen},
		Limit:    maxAgentMemoryInject,
	}
	if channelUUID != "" {
		f.AccessibleChannels = []string{channelUUID}
	}
	if chatGrpID != "" {
		f.AccessibleGrpIDs = []string{chatGrpID}
	}
	items, err := memoryModels.List(ctx, f)
	if err != nil {
		return nil
	}
	out := make([]*memoryModels.MemoryItem, 0, len(items))
	for _, it := range items {
		if it != nil && it.SourceType == AgentMemorySourceType {
			out = append(out, it)
		}
	}
	return out
}
