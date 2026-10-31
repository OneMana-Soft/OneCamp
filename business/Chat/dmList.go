package business

// The DM list a page at a time (DG4). The list was read whole: every DM a
// person ever had, each with its newest message, in one graph read whose
// answer grew with every DM (724 KB at 1,000 DMs; logs/chat-27 probe). A page
// is 50 DMs, newest first (domain/Chat DMPage).
//
// DMs written before the order existed get their last time from one Sweeper
// job, chat.dm-order, a batch at a time until none is left. Until it has
// finished once, a page asked for is the whole list, so no DM goes missing in
// between. A client that asks for no page (one from before DG4) always gets
// the whole list.

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	domain "github.com/akashc777/OneCamp/domain/Chat"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	chatModels "github.com/akashc777/OneCamp/models/postgres/Chat"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	"github.com/google/uuid"
)

const (
	// DMPageSize is how many DMs a page holds: about 40 KB, a sidebar's first
	// screen and more (the probe measured 0.8 KB a DM).
	DMPageSize = 50
	// maxUnreadOffPage bounds the DMs with unread messages that a first page
	// brings along from further down the list.
	maxUnreadOffPage = 200

	// dmOrderKey is the config row that says the backfill finished.
	dmOrderKey = "chat.dm_order_backfill"
	// dmOrderBatch is how many DMs one upsert stamps.
	dmOrderBatch = 500
	// dmOrderBatchesPerRun bounds one run; the next run carries on.
	dmOrderBatchesPerRun = 200
)

// DMListPage is one page of a person's DMs, newest first. user_dms is the
// key the whole list has always had; Next is where the page after starts.
type DMListPage struct {
	UserUUID string                   `json:"user_uuid"`
	DMs      []*dgraphStruct.DgraphDm `json:"user_dms"`
	HasMore  bool                     `json:"has_more"`
	Next     *DMListNext              `json:"next,omitempty"`
}

// DMListNext is the edge a client sends back for the next page.
type DMListNext struct {
	At         time.Time `json:"at"`
	GroupingID string    `json:"grp_id"`
}

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "chat.dm-order", Every: 10 * time.Minute, Run: runDMOrder, RunAtStart: true})
}

// Seams for tests.
var (
	stampUnstampedDMs = domain.StampUnstampedDMs
	dmOrderDone       = func() (bool, error) { return configModel.IsDone(dmOrderKey) }
	markDMOrderDone   = func(note string) error { return configModel.UpsertConfig(dmOrderKey, "done: "+note) }
)

// dmOrderReady is whether this process has seen the backfill finished.
var dmOrderReady atomic.Bool

// runDMOrder stamps the DMs that have no last time, a batch at a time, and
// records when none is left. Run again after that, it does nothing.
func runDMOrder(ctx context.Context) error {
	if dmListPaged() {
		return nil
	}
	total, left := 0, true
	for i := 0; i < dmOrderBatchesPerRun && left; i++ {
		n, err := stampUnstampedDMs(ctx, dmOrderBatch)
		if err != nil {
			return err
		}
		total, left = total+n, n > 0
	}
	if left {
		helpers.LogInfoWithContext(ctx, "business/Chat chat.dm-order: %d DMs ordered this run, more to come", total)
		return nil
	}
	if err := markDMOrderDone(time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	dmOrderReady.Store(true)
	helpers.LogInfoWithContext(ctx, "business/Chat chat.dm-order: the last %d DMs ordered; the DM list now reads in pages", total)
	return nil
}

// dmListPaged is whether the DM list reads in pages: once the backfill has
// finished, as this process saw or the config says.
func dmListPaged() bool {
	if dmOrderReady.Load() {
		return true
	}
	if done, err := dmOrderDone(); err == nil && done {
		dmOrderReady.Store(true)
		return true
	}
	return false
}

// GetUserChatListPage is the page of the person's DMs past edge (the first
// page when nil). A first page also holds the DMs further down with unread
// messages, so their counts show. Until the backfill has finished it is the
// whole list.
func GetUserChatListPage(ctx context.Context, userUUID uuid.UUID, edge *domain.DMEdge) (DMListPage, error) {
	out := DMListPage{UserUUID: userUUID.String()}
	if !dmListPaged() {
		whole, err := GetUserChatListWithLatestChat(ctx, userUUID)
		if whole != nil {
			out.DMs = whole.DMs
		}
		return out, err
	}
	counts, err := domain.GetLatestChatMessageCountByUserID(ctx, userUUID)
	if err != nil {
		return out, err
	}
	var unread []string
	if edge == nil {
		unread = unreadIDs(counts)
	}
	read, unreadDMs, err := domain.DMPage(ctx, userUUID.String(), edge, DMPageSize+1, unread)
	if err != nil {
		return out, err
	}
	page, hasMore := helpers.PageFrom(read, func(d *dgraphStruct.DgraphDm) string { return d.GroupingId }, helpers.PageEdge{}, DMPageSize)
	out.HasMore = hasMore
	if hasMore {
		last := page[len(page)-1]
		out.Next = &DMListNext{At: *last.LastChatAt, GroupingID: last.GroupingId}
	}
	shown := map[string]bool{}
	for _, d := range page {
		shown[d.GroupingId] = true
	}
	for _, d := range unreadDMs {
		if !shown[d.GroupingId] {
			page = append(page, d)
		}
	}
	// A DM whose messages are all deleted has none to show, as the whole list
	// leaves it out; it still moved the edge above, so paging passes it.
	page = slices.DeleteFunc(page, func(d *dgraphStruct.DgraphDm) bool { return len(d.Chats) == 0 })
	out.DMs = conversationDomain.WithoutHidden(ctx, userUUID.String(), withUnread(page, counts))
	return out, nil
}

// unreadIDs is the grouping ids with unread messages, the most recently
// active first, at most maxUnreadOffPage: past the cap, the oldest go.
func unreadIDs(counts map[string]*chatModels.ChatMessageCount) []string {
	var unread []*chatModels.ChatMessageCount
	for _, c := range counts {
		if c != nil && c.ChatCount > 0 {
			unread = append(unread, c)
		}
	}
	slices.SortFunc(unread, func(a, b *chatModels.ChatMessageCount) int {
		if c := b.LastUnreadAt.Time.Compare(a.LastUnreadAt.Time); c != 0 {
			return c
		}
		return strings.Compare(a.GrpId, b.GrpId)
	})
	ids := make([]string, 0, min(len(unread), maxUnreadOffPage))
	for _, c := range unread[:min(len(unread), maxUnreadOffPage)] {
		ids = append(ids, c.GrpId)
	}
	return ids
}

// withUnread sets each DM's unread count from counts (by grouping id).
func withUnread(dms []*dgraphStruct.DgraphDm, counts map[string]*chatModels.ChatMessageCount) []*dgraphStruct.DgraphDm {
	for _, d := range dms {
		d.UnreadMessageCount = 0
		if c := counts[d.GroupingId]; c != nil {
			d.UnreadMessageCount = c.ChatCount
		}
	}
	return dms
}
