package business

// Personal AI briefing — "here's what's happening in your world".
//
// The pull arm of the memory layer: a single, fast, read-only endpoint that
// powers a home-screen "Your briefing" card. It answers the two questions a
// user has when they open the app: "what do I personally own / need to act
// on?" and "what changed recently that I should know about?" — combining the
// structured memory layer (the user's open commitments/questions) with
// recent workspace activity, all permission-scoped.
//
// Production properties:
//   - One bounded call per source, run in PARALLEL, with a hard timeout so
//     the home screen never blocks on AI infra.
//   - Permission-correct: open items are owner-scoped; highlights use the
//     same permission filter as AskAI's recall.
//   - Degrades gracefully: any source that errors/times out is simply
//     omitted; the card still renders what's available.
//   - Fully gated: returns an empty (enabled:false) briefing when AI or the
//     memory layer is off, so the FE can hide the card cleanly.

import (
	"context"
	"sort"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	briefingMaxOpenItems  = 6
	briefingMaxHighlights = 6
	briefingTimeout       = 1200 * time.Millisecond
)

// GetBriefing assembles the user's personal briefing. Never errors on
// partial failure — it returns whatever it could gather within the budget.
func GetBriefing(ctx context.Context, userInfo *userModels.UserInfo) (*adapter.BriefingResponse, error) {
	resp := &adapter.BriefingResponse{
		Enabled:    false,
		OpenItems:  []adapter.MemoryItemView{},
		Highlights: []adapter.BriefingHighlight{},
		DayItems:   []adapter.BriefingDayItem{},
	}

	// Gate: AI + memory layer must be on. (Highlights also need AI; if the
	// layer is off there's nothing structured to show, so we hide the card.)
	settings, err := getAISettingsForMemory(ctx)
	if err != nil || !settings.enabled {
		return resp, nil
	}
	resp.Enabled = true

	bctx, cancel := context.WithTimeout(ctx, briefingTimeout)
	defer cancel()

	type itemsResult struct {
		items []*memoryModels.MemoryItem
	}
	type hlResult struct {
		results []ai.SimilarResult
	}
	itemsCh := make(chan itemsResult, 1)
	hlCh := make(chan hlResult, 1)
	// dayCh carries the cross-connector "Your day" agenda. It runs on the
	// PARENT ctx with its own (longer) internal deadline because these are
	// external API calls; the home screen still never blocks because the
	// final join below is bounded.
	dayCh := make(chan []adapter.BriefingDayItem, 1)
	go func() {
		dayCh <- gatherConnectorDay(ctx, userInfo)
	}()

	channels, projects := getAccessibleResourceUUIDs(userInfo)
	grpIDs := accessibleGroupingIDs(userInfo)
	userUUID := userInfo.UserDgraphInfo.Uuid
	ownerID := userInfo.UserPostgresInfo.Id

	// Open items the user OWNS — only when the memory layer is enabled.
	if settings.memoryEnabled {
		go func() {
			items, lerr := memoryModels.List(bctx, memoryModels.QueryFilter{
				Statuses: []string{memoryModels.StatusOpen},
				OwnerID:  &ownerID,
				Limit:    briefingMaxOpenItems,
			})
			if lerr != nil {
				helpers.LogErrorWithContext(bctx, "briefing: open items failed: %v", lerr)
				itemsCh <- itemsResult{}
				return
			}
			itemsCh <- itemsResult{items: items}
		}()
	} else {
		itemsCh <- itemsResult{}
	}

	// Recent highlights across accessible content (same permission model as
	// AskAI recall). Best-effort.
	go func() {
		res, rerr := searchRecentGlobal(bctx, userInfo, channels, projects, grpIDs, briefingMaxHighlights)
		if rerr != nil {
			helpers.LogInfoWithContext(bctx, "briefing: highlights failed: %v", rerr)
			hlCh <- hlResult{}
			return
		}
		hlCh <- hlResult{results: res}
	}()

	// Collect both, each bounded by the shared deadline.
	resolver := newScopeResolver(userInfo)
	for i := 0; i < 2; i++ {
		select {
		case it := <-itemsCh:
			for _, m := range it.items {
				v := toMemoryView(m)
				resolver.enrich(&v)
				resp.OpenItems = append(resp.OpenItems, v)
			}
		case hl := <-hlCh:
			bot := userBusiness.GetAutomationBot(bctx)
			for _, r := range briefingHighlights(hl.results, bot, userUUID) {
				resp.Highlights = append(resp.Highlights, toBriefingHighlight(r))
			}
		case <-bctx.Done():
			// Budget exhausted — return what we have.
			i = 2
		}
	}

	// Surface the most actionable open items first: overdue commitments,
	// then by recency. (toMemoryView keeps due_at as a YYYY-MM-DD string.)
	sortBriefingOpenItems(resp.OpenItems)

	// Join the cross-connector agenda, bounded so a slow external API can't
	// hold the home screen. gatherConnectorDay already enforces its own
	// internal deadline; this is the hard backstop.
	select {
	case day := <-dayCh:
		resp.DayItems = append(resp.DayItems, day...)
	case <-time.After(connectorBriefingTimeout + 200*time.Millisecond):
		// Connectors too slow this load — ship the rest of the briefing now.
	}
	return resp, nil
}

// sortBriefingOpenItems orders overdue/dated commitments before the rest.
func sortBriefingOpenItems(items []adapter.MemoryItemView) {
	today := time.Now().Format("2006-01-02")
	rank := func(v adapter.MemoryItemView) int {
		if v.Kind == memoryModels.KindCommitment && v.DueAt != "" {
			if v.DueAt < today {
				return 0 // overdue
			}
			return 1 // upcoming due
		}
		if v.Kind == memoryModels.KindCommitment {
			return 2
		}
		return 3
	}
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := rank(items[i]), rank(items[j])
		if ri != rj {
			return ri < rj
		}
		// Within a rank, earlier due date first when both have one.
		if items[i].DueAt != "" && items[j].DueAt != "" {
			return items[i].DueAt < items[j].DueAt
		}
		return items[i].CreatedAt > items[j].CreatedAt
	})
}

// briefingHighlights is what the briefing shows from a recall: news, not
// OneCamp AI talking to this reader. Its DMs to them (the daily note, answers
// to their own questions) are theirs already, and repeating the note under
// "recent highlights" put the same list on the screen twice, one card below
// the other. What the bot said in a shared place is kept, under its display
// name rather than the username the index stores.
func briefingHighlights(results []ai.SimilarResult, bot *userBusiness.BotIdentity, readerUUID string) []ai.SimilarResult {
	if bot == nil || bot.UUID == "" {
		return results
	}
	out := make([]ai.SimilarResult, 0, len(results))
	for _, r := range results {
		if r.ChatByUserID == bot.UUID && r.ChatToUserID == readerUUID {
			continue
		}
		if r.AuthorName == domain.SystemBotUsername() && bot.Name != "" {
			r.AuthorName = bot.Name
		}
		out = append(out, r)
	}
	return out
}

// toBriefingHighlight projects a recall result into the compact card shape.
// highlightText is the one line a highlight shows. A task is indexed as its name,
// a blank line, then its description (EmbedTaskContent), and flattening that to
// plain text ran the two together: "Write the launch announcement Draft for the
// blog". So a task keeps its name and description apart with a separator.
func highlightText(contentType, text string) string {
	if contentType == "task" {
		if name, desc, ok := strings.Cut(text, "\n\n"); ok {
			name, desc = helpers.HTMLToPlainText(name), helpers.HTMLToPlainText(desc)
			if name != "" && desc != "" {
				return name + " · " + desc
			}
		}
	}
	return helpers.HTMLToPlainText(text)
}

func toBriefingHighlight(r ai.SimilarResult) adapter.BriefingHighlight {
	// Defense-in-depth: indexed content can be rich-text/HTML (e.g. doc
	// bodies from the editor). Normalize to clean plain text so the card
	// never shows raw tags like <p class="text-node">.
	snippet := highlightText(r.ContentType, r.ContentText)
	if len([]rune(snippet)) > 160 {
		snippet = string([]rune(snippet)[:160]) + "…"
	}
	return adapter.BriefingHighlight{
		ContentType:  r.ContentType,
		ContentUUID:  r.ContentUUID,
		ChannelUUID:  r.ChannelUUID,
		ChannelName:  r.ChannelName,
		AuthorName:   r.AuthorName,
		Snippet:      snippet,
		ChatGrpID:    r.ChatGrpID,
		ChatByUserID: r.ChatByUserID,
		ChatToUserID: r.ChatToUserID,
		PostUUID:     r.PostUUID,
		TaskUUID:     r.TaskUUID,
		DocUUID:      r.DocUUID,
	}
}
