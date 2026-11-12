package business

import (
	"context"
	"sort"
	"time"

	lastSeenActivityBusiness "github.com/akashc777/OneCamp/business/LastSeenActivity"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	domain "github.com/akashc777/OneCamp/domain/Activity"
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

// FeedQuery is a read of a page of the person's feed.
type FeedQuery struct {
	User     uuid.UUID
	DgraphID string
	// Before is the page's cursor: the time of the last item of the page
	// before it, or now.
	Before time.Time
	Limit  int
	// Notices: the app draws notices ("NOTICE" items), as the Inbox does.
	Notices bool
}

// GetUnifiedActivity is a page of the person's feed before q.Before, newest
// first, with their notices when the app draws them, scored for triage, each
// item marked Unread as the Inbox badge counts it (unreadInInbox), so the Inbox
// draws its dots on exactly what the badge counted. Reading it marks nothing
// seen: Home reads it too, and opening the app cleared the Inbox badge, unseen
// replies and reactions included. The Inbox marks the feed seen by asking
// (MarkActivitySeen).
func GetUnifiedActivity(ctx context.Context, q FeedQuery) (page dgraphModels.UnifiedActivityPagination, err error) {
	page, err = pageOf(ctx, q.User, q.DgraphID, q.Before, q.Limit)
	if err != nil {
		return
	}
	if q.Notices {
		page = WithNotices(ctx, page, q.User.String(), q.Before, q.Limit)
	}
	if page.Activities == nil {
		page.Activities = []dgraphModels.UnifiedActivityItem{}
	}

	// Priority is "what still needs me": what asked the person something and
	// they have since answered drops to low (demoteAnswered), by what they
	// wrote and decided, read once for the page. Best-effort: a read failure
	// leaves priorities as scored. The dots go by what they have read.
	demoteAnswered(page.Activities, readAnswers(ctx, q.User, q.DgraphID, page.Activities))
	l, lerr := readLooks(ctx, q.User, page.Activities)
	if lerr != nil {
		// No dots rather than a guess: the badge, which reads the same last
		// look, isn't counted either.
		helpers.LogErrorWithContext(ctx, "business/GetUnifiedActivity: %s's last look couldn't be read, so nothing is marked unread: %+v", q.User, lerr)
		return
	}
	markUnread(page.Activities, l)
	return
}

// feedPage is a page of the person's feed before beforeTime, newest first:
// the feed's query and its model's filters, cut to limit, then kept to what
// they may read now (readable.go), less the mentions told by a notice
// (dropToldMentions). The feed and the Inbox badge both count from it, so the
// badge never counts what the feed won't show.
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
	page.Activities = dropToldMentions(ctx, userUUID.String(), page.Activities)
	return
}

// unreadActivityCap is where the Inbox badge stops counting: past 99 it reads
// "99+".
const unreadActivityCap = 100

// UnreadActivityCount is the Inbox badge: how many items of the Inbox's
// default view (the feed with its notices) are unread by unreadInInbox, the
// rule the feed marks its items by, up to unreadActivityCap. It counts the
// feed itself (feedPage), so whatever the feed leaves out (deleted messages,
// channels and conversations they aren't in, docs and boards they can't read,
// their own doings) the badge leaves out too. It counted the stored mentions,
// comments and reactions with none of those filters, so a mention in a
// private channel they weren't in, or in a message since deleted, raised a
// badge over an Inbox with nothing new in it; and then the feed without its
// notices, which the Inbox lists (C4), and by the last look alone, so a
// mention read in its channel still counted (C7).
//
// The person's urgent OneCamp AI nudges count by the same rule (UrgentNudges):
// one raised since the last look is unread, and opening the Inbox clears it
// like anything else, though it stays listed until it is resolved or
// dismissed. The web added them to this count on its own, outside the rule,
// so a months-old nudge kept the badge up after every look, and only the
// Priority tab listed it (V-577).
//
// Cheap, as every sidebar asks for it: the stored count bounds the feed's
// part (three graph counts and one indexed notices count since the last
// look), and nothing stored since is nothing of it to read, which is most
// reads. Otherwise one page of the feed and one of bare notices, each at most
// the bound and the cap long, and the conversations' markers when an item is
// in one. The nudges aren't in the stored count: they are one indexed read of
// the person's own open urgent ones, and the last look only when there are
// any.
func UnreadActivityCount(ctx context.Context, userUUID uuid.UUID, userDgraphId string) (uint64, error) {
	upper, err := storedUnread(ctx, userDgraphId, userUUID)
	if err != nil {
		return 0, err
	}
	var items []dgraphModels.UnifiedActivityItem
	if upper > 0 {
		// Each source's items newer than the last look are among its newest
		// `upper`, so a page that long of each holds every one the Inbox
		// would mark.
		limit := int(min(upper, unreadActivityCap))
		before := time.Now().Add(time.Minute)
		page, err := pageOf(ctx, userUUID, userDgraphId, before, limit)
		if err != nil {
			return 0, err
		}
		items = page.Activities
		if NoticeTimes != nil {
			notices, err := NoticeTimes(ctx, userUUID.String(), before, limit)
			if err != nil {
				return 0, err
			}
			items = append(items, notices...)
		}
	}
	if UrgentNudges != nil {
		nudges, err := UrgentNudges(ctx, userUUID, unreadActivityCap)
		if err != nil {
			return 0, err
		}
		items = append(items, nudges...)
	}
	if len(items) == 0 {
		return 0, nil
	}
	l, err := readLooks(ctx, userUUID, items)
	if err != nil {
		return 0, err
	}
	return min(uint64(markUnread(items, l)), unreadActivityCap), nil
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
		item.Priority = ScorePriority(item)
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

// NoticeSource reads a person's inbox notices, newest first: items with no
// graph record of their own (a mention in a doc's body, "shared with you", a
// proposal), from before `before`, at most limit of them, each with its time
// set. business/Follow sets it from its init; until then there are none.
var NoticeSource func(ctx context.Context, userUUID string, before time.Time, limit int) ([]dgraphModels.UnifiedActivityItem, error)

// WithNotices is a page of the feed with the person's notices merged in, by
// time. A source that had more than it gave can only be merged down to its
// oldest item given: anything past that it hasn't shown yet, and the next page
// (before the last item here) asks both sources again.
func WithNotices(ctx context.Context, page dgraphModels.UnifiedActivityPagination, userUUID string, before time.Time, limit int) dgraphModels.UnifiedActivityPagination {
	if NoticeSource == nil || limit <= 0 {
		return page
	}
	notices, err := NoticeSource(ctx, userUUID, before, limit+1)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/WithNotices Failed to read the notices err: %+v", err)
		return page
	}
	return mergeNotices(page, notices, limit)
}

// mergeNotices merges notices (up to limit+1 of them, newest first) into a page
// of at most limit items. Pure.
func mergeNotices(page dgraphModels.UnifiedActivityPagination, notices []dgraphModels.UnifiedActivityItem, limit int) dgraphModels.UnifiedActivityPagination {
	if len(notices) == 0 {
		return page
	}
	at := func(item dgraphModels.UnifiedActivityItem) time.Time {
		t, _ := time.Parse(activityTimeLayout, item.Time)
		return t
	}
	noticesMore := len(notices) > limit
	if noticesMore {
		notices = notices[:limit]
	}
	// The newest time below which a source may hold items it hasn't given.
	var floor time.Time
	if page.HasMore && len(page.Activities) > 0 {
		floor = at(page.Activities[len(page.Activities)-1])
	}
	if noticesMore {
		if last := at(notices[len(notices)-1]); last.After(floor) {
			floor = last
		}
	}
	all := append(append([]dgraphModels.UnifiedActivityItem{}, page.Activities...), notices...)
	sort.SliceStable(all, func(i, j int) bool { return at(all[i]).After(at(all[j])) })
	out := dgraphModels.UnifiedActivityPagination{HasMore: page.HasMore || noticesMore, Activities: []dgraphModels.UnifiedActivityItem{}}
	for _, item := range all {
		if len(out.Activities) == limit || (!floor.IsZero() && at(item).Before(floor)) {
			out.HasMore = true
			break
		}
		out.Activities = append(out.Activities, item)
	}
	return out
}

func PublishActivityToUser(recipientUserUUID string, activityItem *dgraphModels.UnifiedActivityItem) {
	// Score priority on the realtime path too so live notifications carry
	// the same triage signal as the fetched feed.
	if activityItem != nil && activityItem.Priority == "" {
		activityItem.Priority = ScorePriority(*activityItem)
	}
	mqttActivity := &mqttStruct.MqttActivity{
		UserUuid: recipientUserUUID,
		Activity: activityItem,
	}

	helpers.GoSafeNamed("mqttBusiness.PublishActivity", func() { mqttBusiness.PublishActivity(mqttActivity, recipientUserUUID) })
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
