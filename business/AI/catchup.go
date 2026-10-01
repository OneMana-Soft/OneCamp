package business

// "Catch me up" — an AI summary of exactly what the user missed in a scope
// since they last looked at it.
//
// This is the PUSH counterpart to AskAI's pull: instead of "what do I want
// to know", it answers "what changed while I was away" the moment a user
// opens a busy channel/DM or the app itself. It is the single highest-value
// answer to Slack's core pain — too many unread messages, no idea what
// matters.
//
// How it stays cheap and correct:
//   - The unread boundary is the user's own last-seen timestamp for the
//     scope (per-channel / per-chat), floored by a lookback so an
//     ancient/absent last-seen can't summarize months of history.
//   - The unread window is pulled from the SAME permission-scoped embeddings
//     index the rest of AI uses (FetchContentSince), so it never crosses an
//     access boundary and needs no separate content store.
//   - Nothing unread → returns HasUnread=false immediately (no LLM call),
//     so the FE hides the surface and a quiet workspace costs ~one cheap
//     query.
//   - Fully gated on AI being enabled; resiliency (rate limit + circuit
//     breaker) is shared with the other AI features.
//   - The user's OWN open commitments/decisions in the scope (from the
//     memory layer) are woven in when available, so catch-up also says
//     "and here's what you now owe".

import (
	"context"
	"fmt"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	lastSeenChannelDomain "github.com/akashc777/OneCamp/domain/LastSeenChannel"
	lastSeenChatDomain "github.com/akashc777/OneCamp/domain/LastSeenChat"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	// catchUpLookbackFloor bounds how far back catch-up will ever reach,
	// even when the user has no last-seen row (new member) or an ancient
	// one. Keeps "what did I miss" about the recent past, not all history.
	catchUpLookbackFloor = 14 * 24 * time.Hour

	// catchUpMaxItems caps how many unread items feed the summary. The
	// prompt budget (TruncateToTokenBudget) is the hard backstop; this keeps
	// the OpenSearch fetch and prompt bounded on very busy scopes.
	catchUpMaxItems = 80

	// catchUpMaxMemoryItems bounds the user's own open items woven into the
	// catch-up so the prompt stays high-signal.
	catchUpMaxMemoryItems = 5
)

// catchUpScopeChannel / Chat / Workspace are the supported scope types.
const (
	catchUpScopeChannel   = "channel"
	catchUpScopeChat      = "chat"
	catchUpScopeWorkspace = "workspace"
)

// catchUpSystemPrompt tunes the model for a "while you were away" recap:
// short, skimmable, oriented around what changed and what the reader should
// act on — not a generic summary.
const catchUpSystemPrompt = `You are OneCamp's AI assistant. The user has been away and wants to know what they missed.

You are given the messages they have NOT yet seen, in chronological order.

Write a tight "while you were away" recap:
- Lead with what matters: decisions made, questions aimed at the user, things needing a reply or action.
- Group related points; use short bullets. Keep it skimmable.
- Attribute with @names when it helps ("@alex asked ...").
- Maximum 150 words. If there are clear action items for the reader, end with "📋 For you:" and list them.
- Be strictly factual — only use what's in the messages. Never invent.
- No greetings or filler. If the messages are trivial chatter, say so in one line.`

// GetCatchUp builds the "what did I miss" recap for a scope. Never errors on
// an empty/quiet scope — it returns HasUnread=false so the caller hides the
// surface. Permission is enforced by the caller (scope access) AND by the
// permission-filtered retrieval.
func GetCatchUp(ctx context.Context, userInfo *userModels.UserInfo, req adapter.CatchUpRequest) (*adapter.CatchUpResponse, error) {
	resp := &adapter.CatchUpResponse{
		Enabled:   false,
		HasUnread: false,
		ScopeType: req.ScopeType,
	}

	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return resp, nil // AI off → caller hides the surface
	}
	resp.Enabled = true

	// Resolve the unread boundary (since when) + the scope's retrieval
	// filter, validating the user actually has access to the scope.
	since, filterType, filterValue, scopeName, err := catchUpBoundary(ctx, userInfo, req)
	if err != nil {
		return nil, err
	}
	resp.ScopeName = scopeName
	resp.SinceISO = since.Format(time.RFC3339)

	// Pull the unread window from the permission-scoped embeddings index.
	channels, projects := getAccessibleResourceUUIDs(userInfo)
	grpIDs := accessibleGroupingIDs(userInfo)
	userUUID := userInfo.UserDgraphInfo.Uuid

	// State the boundary the retrieval below is confined to, before anything is
	// read, so a recap can never report a summary without also reporting how far
	// it was allowed to look.
	resp.ScopesAllowed = scopesAllowed(req.ScopeType, channels, projects, grpIDs)

	var items []ai.ScopedContent
	if req.ScopeType == catchUpScopeWorkspace {
		items, err = ai.FetchUnreadAcrossScopes(ctx, userUUID, channels, projects, grpIDs, since.Unix(), catchUpMaxItems)
	} else {
		items, err = ai.FetchContentSince(ctx, filterType, filterValue, since.Unix(), catchUpMaxItems)
	}
	if err != nil {
		return nil, fmt.Errorf("catch-up fetch failed: %w", err)
	}

	if len(items) == 0 {
		// Nothing missed — the happy "you're all caught up" path. No LLM call.
		return resp, nil
	}
	resp.HasUnread = true
	resp.MessageCount = len(items)

	// Resiliency: only now, when we know there's real work, do we spend an
	// LLM call against the rate limiter + circuit breaker.
	if err := svc.Resiliency.PreCheck(ctx, userUUID); err != nil {
		return nil, err
	}

	content := formatContentWindow(items)
	// Weave in the reader's OWN open items in this scope (best-effort).
	if mem := catchUpOwnedItems(ctx, userInfo, req); mem != "" {
		content = mem + "\n" + content
	}
	// Hard token ceiling so a very busy scope can't overflow the model window.
	catchupLimits := ai.LimitsFrom(ctx)
	content = catchupLimits.TruncateForPrompt(ctx, content, catchupLimits.ContextBudget())

	summary, err := svc.Summarize(ctx, content, catchUpSystemPrompt)
	if err != nil {
		svc.Resiliency.CB.RecordResult(err)
		return nil, fmt.Errorf("catch-up summarization failed: %w", err)
	}
	svc.Resiliency.CB.RecordSuccess()

	resp.Summary = SanitizeResponse(summary)
	resp.Provider = string(svc.Config.Provider())
	return resp, nil
}

// scopesAllowed reports how many conversations the recap was permitted to read.
//
// One for a single-scope recap, by definition rather than by counting:
// catchUpBoundary has already refused the request outright if the reader is not
// in that channel or conversation, so a recap that reaches here has exactly one
// scope open to it.
//
// For the workspace recap it is the size of the membership that
// FetchUnreadAcrossScopes is filtered to — every channel, conversation and
// project the reader belongs to, which is precisely the set the query may touch.
//
// Free of the user model on purpose: the caller has already resolved the three
// lists, and taking them as arguments is what lets this be tested without a
// database behind it.
func scopesAllowed(scopeType string, channels, projects, groups []string) int {
	if scopeType != catchUpScopeWorkspace {
		return 1
	}
	return len(channels) + len(projects) + len(groups)
}

// catchUpBoundary resolves (sinceTime, filterType, filterValue, scopeName)
// for the request, enforcing scope access. The boundary is the user's
// last-seen for the scope, floored by catchUpLookbackFloor.
func catchUpBoundary(ctx context.Context, userInfo *userModels.UserInfo, req adapter.CatchUpRequest) (time.Time, string, string, string, error) {
	floor := time.Now().Add(-catchUpLookbackFloor)
	userID := userInfo.UserPostgresInfo.Id

	switch req.ScopeType {
	case catchUpScopeChannel:
		chUUID := strings.TrimSpace(req.ChannelUUID)
		if _, err := uuid.Parse(chUUID); err != nil {
			return time.Time{}, "", "", "", fmt.Errorf("channel_uuid must be a UUID")
		}
		// Access check: the user must be a member of the channel.
		channels, _ := getAccessibleResourceUUIDs(userInfo)
		if !containsStr(channels, chUUID) {
			return time.Time{}, "", "", "", fmt.Errorf("not authorized for this channel")
		}
		name := channelNameFor(userInfo, chUUID)
		chID, _ := uuid.Parse(chUUID)
		last, ok, err := lastSeenChannelDomain.GetLastSeenChannel(ctx, userID, chID)
		if err != nil {
			return time.Time{}, "", "", "", err
		}
		return floorSince(last, ok, floor), "channel_uuid", chUUID, name, nil

	case catchUpScopeChat:
		grp := strings.TrimSpace(req.ChatGrpID)
		// DMs pass the peer's user UUID; derive the deterministic grouping
		// id server-side (mirrors SummarizeDM) so the FE never has to.
		if grp == "" && strings.TrimSpace(req.ToUserUUID) != "" {
			grp = helpers.GetGroupingId(userInfo.UserPostgresInfo.Id.String(), strings.TrimSpace(req.ToUserUUID))
		}
		if grp == "" {
			return time.Time{}, "", "", "", fmt.Errorf("chat_grp_id or to_user_uuid is required")
		}
		if !containsStr(accessibleGroupingIDs(userInfo), grp) {
			return time.Time{}, "", "", "", fmt.Errorf("not authorized for this conversation")
		}
		last, ok, err := lastSeenChatDomain.GetLastSeenChat(ctx, userID, grp)
		if err != nil {
			return time.Time{}, "", "", "", err
		}
		return floorSince(last, ok, floor), "chat_grp_id", grp, "", nil

	case catchUpScopeWorkspace, "":
		// Workspace-wide: no single last-seen, so use the lookback floor.
		// FetchUnreadAcrossScopes handles the multi-scope fan-in.
		return floor, "", "", "", nil

	default:
		return time.Time{}, "", "", "", fmt.Errorf("invalid scope_type %q", req.ScopeType)
	}
}

// floorSince returns the later of the user's last-seen and the lookback
// floor. A missing last-seen (ok=false) or an ancient one collapses to the
// floor, so catch-up never summarizes more than catchUpLookbackFloor.
func floorSince(last time.Time, ok bool, floor time.Time) time.Time {
	if !ok || last.Before(floor) {
		return floor
	}
	return last
}

// channelNameFor resolves a channel's display name from the user's sidebar
// graph info (no extra query). Empty when not found.
func channelNameFor(userInfo *userModels.UserInfo, chUUID string) string {
	for _, ch := range userInfo.UserDgraphInfo.Channels {
		if ch.Uuid == chUUID {
			return ch.Name
		}
	}
	return ""
}

// formatContentWindow renders content items as chronological "Author: text"
// lines (oldest first) so the recap reads naturally. FetchContentSince
// already returns ascending order.
func formatContentWindow(items []ai.ScopedContent) string {
	var sb strings.Builder
	for _, it := range items {
		text := strings.TrimSpace(it.ContentText)
		if text == "" {
			continue
		}
		name := strings.TrimSpace(it.AuthorName)
		if name == "" {
			name = "participant"
		}
		sb.WriteString(name)
		sb.WriteString(": ")
		sb.WriteString(text)
		sb.WriteString("\n")
	}
	return sb.String()
}

// catchUpOwnedItems renders the reader's OWN open commitments/questions in
// the requested scope as a compact block, so the recap can highlight what
// they now owe. Best-effort + intent-free: returns "" when the memory layer
// is off, the scope is workspace-wide, or there's nothing open.
func catchUpOwnedItems(ctx context.Context, userInfo *userModels.UserInfo, req adapter.CatchUpRequest) string {
	settings, err := getAISettingsForMemory(ctx)
	if err != nil || !settings.memoryEnabled {
		return ""
	}
	ownerID := userInfo.UserPostgresInfo.Id
	filter := memoryModels.QueryFilter{
		Statuses: []string{memoryModels.StatusOpen},
		OwnerID:  &ownerID,
		Limit:    catchUpMaxMemoryItems,
	}
	switch req.ScopeType {
	case catchUpScopeChannel:
		filter.AccessibleChannels = []string{strings.TrimSpace(req.ChannelUUID)}
	case catchUpScopeChat:
		filter.AccessibleGrpIDs = []string{strings.TrimSpace(req.ChatGrpID)}
	default:
		return "" // workspace-wide recap leans on the briefing for owned items
	}

	items, err := memoryModels.List(ctx, filter)
	if err != nil || len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Your open items in this conversation (from workspace memory)\n")
	for _, it := range items {
		line := strings.TrimSpace(it.Content)
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		sb.WriteString("- [")
		sb.WriteString(it.Kind)
		sb.WriteString("] ")
		sb.WriteString(line)
		if it.DueAt != nil {
			sb.WriteString(" (due ")
			sb.WriteString(it.DueAt.Format("2006-01-02"))
			sb.WriteString(")")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	return sb.String()
}
