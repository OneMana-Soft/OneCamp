package business

import (
	"context"
	"sort"
	"time"

	lastSeenActivityBusiness "github.com/akashc777/OneCamp/business/LastSeenActivity"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	domain "github.com/akashc777/OneCamp/domain/Activity"
	lastSeenActivityDomain "github.com/akashc777/OneCamp/domain/LastSeenActivity"
	lastSeenChannelDomain "github.com/akashc777/OneCamp/domain/LastSeenChannel"
	lastSeenChatDomain "github.com/akashc777/OneCamp/domain/LastSeenChat"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/google/uuid"
)

type MentionPagination struct {
	Mentions []*dgraphStruct.DgraphMentions `json:"mentions,omitempty"`
	HasMore  bool                           `json:"has_more"`
}

type CommentPagination struct {
	Comments []*dgraphStruct.DgraphComment `json:"comments,omitempty"`
	HasMore  bool                          `json:"has_more"`
}

type ReactionPagination struct {
	Reactions []*dgraphModels.ReactionsActivity `json:"reactions,omitempty"`
	HasMore   bool                              `json:"has_more"`
}

// GetMentionsByUserId is a page of the mentions of the person (by graph uid
// and by uuid), the mentions tab, without those on a doc or a board they
// can't read now (readable.go).
func GetMentionsByUserId(ctx context.Context, userDgraphId string, userUUID string, pageIndex int, pageSize int) (mentionsPage MentionPagination, err error) {
	dgraphMentions, actualLen, err := domain.GetLatestMentionByUserId(ctx, userDgraphId, pageIndex, pageSize)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetMentionsByUserId Failed to get user's mention err: %+v",
			err)
		return
	}

	if len(dgraphMentions) > pageSize {
		mentionsPage.Mentions = dgraphMentions[:pageSize]
	} else {
		mentionsPage.Mentions = dgraphMentions
	}
	mentionsPage.Mentions = keepReadable(ctx, userUUID, mentionsPage.Mentions, mentionAbout)

	mentionsPage.HasMore = actualLen > pageSize

	return
}

// GetCommentsByUserId is a page of the comments on what the person added,
// without those on a doc or a board they can't read now.
func GetCommentsByUserId(ctx context.Context, userDgraphId string, userUUID string, pageIndex int, pageSize int) (commentsPage CommentPagination, err error) {
	dgraphComments, actualLen, err := domain.GetLatestCommentActivityByUserId(ctx, userDgraphId, pageIndex, pageSize)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetMentionsByUserId Failed to user's comment err: %+v",
			err)
		return
	}

	if len(dgraphComments) > pageSize {
		commentsPage.Comments = dgraphComments[:pageSize]
	} else {
		commentsPage.Comments = dgraphComments
	}
	commentsPage.Comments = keepReadable(ctx, userUUID, commentsPage.Comments, commentAbout)

	commentsPage.HasMore = actualLen > pageSize

	return
}

// GetReactionsByUserId is a page of the reactions to what the person added,
// without those on a comment on a doc or a board they can't read now.
func GetReactionsByUserId(ctx context.Context, userDgraphId string, userUUID string, pageIndex int, pageSize int) (reactionPage ReactionPagination, err error) {
	dgraphReaction, actualLen, err := domain.GetReactionsByUserId(ctx, userDgraphId, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetReactionsByUserId Failed to user's reaction err: %+v",
			err)
		return
	}

	if len(dgraphReaction) > pageSize {
		reactionPage.Reactions = dgraphReaction[:pageSize]
	} else {
		reactionPage.Reactions = dgraphReaction
	}
	reactionPage.Reactions = keepReadable(ctx, userUUID, reactionPage.Reactions, reactionAbout)
	reactionPage.HasMore = actualLen > pageSize

	return
}

// GetUnifiedActivity is a page of the person's feed before beforeTime, newest
// first, scored for triage. Reading it marks nothing seen: Home reads it too,
// and opening the app cleared the Inbox badge, unseen replies and reactions
// included. The Inbox marks the feed seen by asking (MarkActivitySeen).
func GetUnifiedActivity(ctx context.Context, userUUID uuid.UUID, userDgraphId string, beforeTime time.Time, limit int) (dgraphActivities dgraphModels.UnifiedActivityPagination, err error) {

	dgraphActivities, err = feedPage(ctx, userUUID, userDgraphId, beforeTime, limit)
	if err != nil {
		return
	}

	// Demote items the user has ALREADY engaged with (opened the
	// conversation since the mention/comment) so the Priority view reflects
	// "what still needs me", not "what ever needed me". Read-state is the
	// signal; two batched last-seen reads keep this cheap. Best-effort: a
	// read failure simply leaves priorities as content-scored.
	//
	// Skip the two reads entirely when there's nothing demotable (e.g. a
	// reaction-only page) so a quiet feed costs zero extra queries.
	if hasDemotableActivity(dgraphActivities.Activities) {
		if chSeen, csErr := lastSeenChannelDomain.GetAllLastSeenChannelsForUser(ctx, userUUID); csErr == nil {
			grpSeen, _ := lastSeenChatDomain.GetAllLastSeenChatsForUser(ctx, userUUID)
			demoteSeenActivities(dgraphActivities.Activities, chSeen, grpSeen)
		} else {
			helpers.LogErrorWithContext(ctx, "GetUnifiedActivity: last-seen read for triage failed: %v", csErr)
		}
	}

	// Safety check: if slice is nil, return empty slice
	if dgraphActivities.Activities == nil {
		dgraphActivities.Activities = []dgraphModels.UnifiedActivityItem{}
	}

	return
}

// feedPage is a page of the person's feed before beforeTime, newest first:
// the feed's query and its model's filters, cut to limit, then kept to what
// they may read now (readable.go). The feed and the Inbox badge both count
// from it, so the badge never counts what the feed won't show.
func feedPage(ctx context.Context, userUUID uuid.UUID, userDgraphId string, beforeTime time.Time, limit int) (page dgraphModels.UnifiedActivityPagination, err error) {
	mentions, comments, reactions, err := domain.GetUnifiedActivity(ctx, userDgraphId, beforeTime, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetUnifiedActivity Failed to get activity data from domain err: %+v", err)
		return
	}

	allActivities := mergeActivities(mentions, comments, reactions)

	// Truncate to limit FIRST (ordering is purely by time, not priority, so
	// truncation is independent of demotion). We then demote only the items
	// that actually survive into the page — no wasted work on discarded rows.
	if len(allActivities) > limit {
		page.Activities = allActivities[:limit]
		page.HasMore = true
	} else {
		page.Activities = allActivities
		page.HasMore = false
	}
	// After the page is cut: whether there is more is what the graph said, so
	// leaving an item out makes a page short, never the last. The person's
	// graph uuid is their account id.
	page.Activities = keepReadable(ctx, userUUID.String(), page.Activities, itemAbout)
	return
}

// unreadActivityCap is where the Inbox badge stops counting: past 99 it reads
// "99+".
const unreadActivityCap = 100

// UnreadActivityCount is the Inbox badge: how many items of the person's feed
// are newer than when they last marked it seen, up to unreadActivityCap. It
// counts the feed itself (feedPage), so whatever the feed leaves out (deleted
// messages, channels and conversations they aren't in, docs and boards they
// can't read, their own doings) the badge leaves out too. It counted the
// stored mentions, comments and reactions with none of those filters, so a
// mention in a private channel they weren't in, or in a message since
// deleted, raised a badge over an Inbox with nothing new in it.
func UnreadActivityCount(ctx context.Context, userUUID uuid.UUID, userDgraphId string) (uint64, error) {
	// The stored count is an upper bound for three counts' cost: nothing
	// stored since they looked is nothing in the feed, which is most reads.
	upper, err := lastSeenActivityBusiness.GetTotalUnreadActivityCount(ctx, userDgraphId, userUUID)
	if err != nil || upper == 0 {
		return 0, err
	}
	lastSeen, err := lastSeenActivityDomain.GetLastSeenActivityByUserId(ctx, userUUID)
	if err != nil {
		return 0, err
	}
	// Each kind of item newer than the last look is among that kind's newest
	// `upper`, so a page that long holds every one the feed would show.
	page, err := feedPage(ctx, userUUID, userDgraphId, time.Now().Add(time.Minute), int(min(upper, unreadActivityCap)))
	if err != nil {
		return 0, err
	}
	var n uint64
	for _, it := range page.Activities {
		if at, perr := time.Parse(activityTimeLayout, it.Time); perr == nil && at.After(lastSeen) {
			n++
		}
	}
	return n, nil
}

// MarkActivitySeen is the Inbox opened: everything in the feed until now is
// seen, and the badge drops to what arrives next. Only the Inbox asks for it
// (POST /activity/seen); reading the feed marks nothing.
func MarkActivitySeen(ctx context.Context, userUUID uuid.UUID) error {
	return lastSeenActivityBusiness.CreateOrUpdateLastSeenActivity(ctx, userUUID)
}

// activityTimeLayout is how an activity's time, and so the feed's beforeTime
// cursor, is written: to the nanosecond. Cut to the second, a cursor taken
// from the last item of a page skipped every older item within that second
// (the next page asks for items before the cursor). A cursor an older client
// sends in whole seconds still reads, as time.Parse takes either.
const activityTimeLayout = time.RFC3339Nano

// activityTime is t as an activity's time, or "" without one.
func activityTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(activityTimeLayout)
}

// mergeActivities is the feed's items, newest first, each scored and with its
// actor's kind. Ordered by the times themselves: RFC 3339 with fractions of a
// second drops trailing zeros, so the strings no longer sort as the times do
// ("…13Z" would sort after "…13.5Z").
func mergeActivities(mentions []*dgraphStruct.DgraphMentions, comments []*dgraphStruct.DgraphComment, reactions []*dgraphModels.ReactionsActivity) []dgraphModels.UnifiedActivityItem {
	type timed struct {
		item dgraphModels.UnifiedActivityItem
		at   time.Time
	}
	all := make([]timed, 0, len(mentions)+len(comments)+len(reactions))
	add := func(item dgraphModels.UnifiedActivityItem, at *time.Time) {
		item.Time = activityTime(at)
		item.Priority = scoreActivityPriority(item)
		item.ActorKind = ActorKind(actorOf(&item))
		t := time.Time{}
		if at != nil {
			t = *at
		}
		all = append(all, timed{item: item, at: t})
	}
	for _, m := range mentions {
		add(dgraphModels.UnifiedActivityItem{ActivityType: "MENTION", Mention: m}, m.CreatedAt)
	}
	for _, c := range comments {
		add(dgraphModels.UnifiedActivityItem{ActivityType: "COMMENT", Comment: c}, c.CreatedAt)
	}
	for _, r := range reactions {
		add(dgraphModels.UnifiedActivityItem{ActivityType: "REACTION", Reaction: r}, r.AddedAt)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	out := make([]dgraphModels.UnifiedActivityItem, len(all))
	for i := range all {
		out[i] = all[i].item
	}
	return out
}

func PublishActivityToUser(recipientUserUUID string, activityItem *dgraphModels.UnifiedActivityItem) {
	// Score priority on the realtime path too so live notifications carry
	// the same triage signal as the fetched feed.
	if activityItem != nil && activityItem.Priority == "" {
		activityItem.Priority = scoreActivityPriority(*activityItem)
	}
	mqttActivity := &mqttStruct.MqttActivity{
		UserUuid: recipientUserUUID,
		Activity: activityItem,
	}

	go mqttBusiness.PublishActivity(mqttActivity, recipientUserUUID)
}

// Who an activity item's actor is, for the people / agents / apps filter.
const (
	ActorPerson = "person"
	ActorAgent  = "agent"
	ActorApp    = "app"
)

// actorOf is the principal who did what an activity item reports.
func actorOf(item *dgraphModels.UnifiedActivityItem) *dgraphStruct.DgraphUser {
	switch {
	case item.Mention != nil && item.Mention.Post != nil:
		return item.Mention.Post.PostBy
	case item.Mention != nil && item.Mention.Comment != nil:
		return item.Mention.Comment.CommentBy
	case item.Mention != nil && item.Mention.Chat != nil:
		return item.Mention.Chat.From
	case item.Comment != nil:
		return item.Comment.CommentBy
	case item.Reaction != nil:
		return item.Reaction.AddedBy
	}
	return nil
}

// ActorKind sorts an actor into people, agents and apps, and drops the email
// that classifying it needed, which the reader has no use for. The assistant
// and configured agents are agents; the Slack bridge and channel guests carry
// people's own words, so they count as people; automations and any bot this
// build doesn't know are apps.
func ActorKind(u *dgraphStruct.DgraphUser) string {
	if u == nil {
		return ActorPerson
	}
	email := u.EmailID
	u.EmailID = ""
	if !u.IsBot {
		return ActorPerson
	}
	switch userDomain.ClassifyBot(email) {
	case userDomain.BotKindAssistant, userDomain.BotKindAgent:
		return ActorAgent
	case userDomain.BotKindBridge, userDomain.BotKindGuest:
		return ActorPerson
	default:
		return ActorApp
	}
}
