package business

import (
	"context"
	"fmt"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	"github.com/google/uuid"
)

// The Inbox badge (owner's bug, 5 Nov: "the badge stays, even on clicking
// Inbox, and nothing in the Inbox says which item it counts"). One rule says
// whether an item is unread, the badge counts the Inbox's default view by it
// (the feed with its notices), and each item the feed lists carries it, so the
// Inbox draws a dot on exactly the items the badge counted.

var lookAt = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

func at(d time.Duration) string { return lookAt.Add(d).Format(activityTimeLayout) }

func inChannel(ch string, d time.Duration) dgraphModels.UnifiedActivityItem {
	return dgraphModels.UnifiedActivityItem{ActivityType: "MENTION", Time: at(d), Priority: PriorityNormal,
		Mention: &dgraphStruct.DgraphMentions{Post: &dgraphStruct.DgraphPost{Channel: &dgraphStruct.DgraphChannel{Uuid: ch}}}}
}

func inChat(grp string, d time.Duration) dgraphModels.UnifiedActivityItem {
	return dgraphModels.UnifiedActivityItem{ActivityType: "COMMENT", Time: at(d), Priority: PriorityNormal,
		Comment: &dgraphStruct.DgraphComment{ChatGroupingId: grp}}
}

func noticeAt(id string, d time.Duration) dgraphModels.UnifiedActivityItem {
	return dgraphModels.UnifiedActivityItem{ActivityType: "NOTICE", Time: at(d), Priority: PriorityNormal,
		Notice: &dgraphModels.NoticeItem{ID: id, Kind: "doc.shared"}}
}

func reactionAt(d time.Duration) dgraphModels.UnifiedActivityItem {
	return dgraphModels.UnifiedActivityItem{ActivityType: "REACTION", Time: at(d), Priority: PriorityLow,
		Reaction: &dgraphModels.ReactionsActivity{}}
}

func TestUnreadInInboxIsNewSinceTheLastLookAndNotReadWhereItIs(t *testing.T) {
	l := looks{
		inbox:    lookAt,
		channels: map[string]time.Time{"read-since": lookAt.Add(20 * time.Minute), "read-before": lookAt.Add(5 * time.Minute)},
		chats:    map[string]time.Time{"grp-read": lookAt.Add(time.Hour)},
	}
	for _, c := range []struct {
		name string
		item dgraphModels.UnifiedActivityItem
		want bool
	}{
		{"a mention since the last look, in a channel they haven't read since", inChannel("unread", 10*time.Minute), true},
		{"a mention before the last look", inChannel("unread", -10*time.Minute), false},
		{"a mention at the last look itself", inChannel("unread", 0), false},
		{"a mention they read in its channel since (C7)", inChannel("read-since", 10*time.Minute), false},
		{"a mention after they last read its channel", inChannel("read-before", 10*time.Minute), true},
		{"a reply they read in its conversation since (C7)", inChat("grp-read", 10*time.Minute), false},
		{"a reply in a conversation they haven't read", inChat("grp-other", 10*time.Minute), true},
		{"a notice since the last look (C4)", noticeAt("n1", time.Minute), true},
		{"a notice before the last look", noticeAt("n2", -time.Minute), false},
		{"a reaction since the last look", reactionAt(time.Minute), true},
		{"an item with no time", dgraphModels.UnifiedActivityItem{ActivityType: "MENTION"}, false},
	} {
		if got := unreadInInbox(&c.item, l); got != c.want {
			t.Errorf("%s: unread %v, want %v", c.name, got, c.want)
		}
	}
	// Someone who never opened their Inbox has everything unread.
	if item := inChannel("unread", -48*time.Hour); !unreadInInbox(&item, looks{}) {
		t.Error("an item for someone who never opened their Inbox isn't unread")
	}
}

// badgeWorld stands in for the stores the badge reads: the stored upper bound,
// the last look, the conversations' markers, the feed and the notices.
type badgeWorld struct {
	upper   uint64
	feed    []dgraphModels.UnifiedActivityItem
	notices []dgraphModels.UnifiedActivityItem
	reads   int
}

func (w *badgeWorld) install(t *testing.T) {
	t.Helper()
	savedStored, savedLook, savedMarkers, savedPage := storedUnread, lastLookOf, markersOf, pageOf
	savedTimes, savedSource, savedNudges := NoticeTimes, NoticeSource, UrgentNudges
	t.Cleanup(func() {
		storedUnread, lastLookOf, markersOf, pageOf = savedStored, savedLook, savedMarkers, savedPage
		NoticeTimes, NoticeSource, UrgentNudges = savedTimes, savedSource, savedNudges
	})
	storedUnread = func(context.Context, string, uuid.UUID) (uint64, error) { return w.upper, nil }
	lastLookOf = func(context.Context, uuid.UUID) (time.Time, error) { return lookAt, nil }
	markersOf = func(context.Context, uuid.UUID) (map[string]time.Time, map[string]time.Time, error) {
		return map[string]time.Time{"read-since": lookAt.Add(30 * time.Minute)}, map[string]time.Time{"grp-read": lookAt.Add(time.Hour)}, nil
	}
	pageOf = func(_ context.Context, _ uuid.UUID, _ string, _ time.Time, limit int) (dgraphModels.UnifiedActivityPagination, error) {
		w.reads++
		items := append([]dgraphModels.UnifiedActivityItem{}, w.feed...)
		if len(items) > limit {
			return dgraphModels.UnifiedActivityPagination{Activities: items[:limit], HasMore: true}, nil
		}
		return dgraphModels.UnifiedActivityPagination{Activities: items}, nil
	}
	notices := func(_ context.Context, _ string, _ time.Time, limit int) ([]dgraphModels.UnifiedActivityItem, error) {
		items := append([]dgraphModels.UnifiedActivityItem{}, w.notices...)
		return items[:min(limit, len(items))], nil
	}
	NoticeTimes, NoticeSource = notices, notices
}

// The Inbox's default view in this world: two mentions since the last look
// (one read in its channel since), a reply read in its conversation, a
// reaction since, an older mention, and four notices, three of them new.
func inboxWorld() *badgeWorld {
	return &badgeWorld{
		upper: 9,
		feed: []dgraphModels.UnifiedActivityItem{
			inChannel("unread", 25*time.Minute),
			inChannel("read-since", 20*time.Minute),
			inChat("grp-read", 15*time.Minute),
			reactionAt(10 * time.Minute),
			inChannel("unread", -time.Hour),
		},
		notices: []dgraphModels.UnifiedActivityItem{
			noticeAt("n-new", 12*time.Minute),
			noticeAt("n-newer", 22*time.Minute),
			noticeAt("n-since", 5*time.Minute),
			noticeAt("n-old", -2*time.Hour),
		},
	}
}

func TestTheBadgeCountsTheInboxsUnreadItems(t *testing.T) {
	w := inboxWorld()
	w.install(t)
	got, err := UnreadActivityCount(context.Background(), uuid.New(), "0x1")
	if err != nil {
		t.Fatal(err)
	}
	// The new mention in an unread channel, the reaction and the three new
	// notices: not the mention and the reply read where they are (C7), nor
	// anything before the last look.
	if got != 5 {
		t.Errorf("the badge counts %d; want 5: notices are counted (C4), what was read in its conversation isn't (C7), and only what's newer than the last look", got)
	}
}

func TestTheFeedMarksExactlyWhatTheBadgeCounts(t *testing.T) {
	w := inboxWorld()
	w.install(t)
	me := uuid.New()
	page, err := GetUnifiedActivity(context.Background(), FeedQuery{User: me, DgraphID: "0x1", Before: lookAt.Add(time.Hour), Limit: 20, Notices: true})
	if err != nil {
		t.Fatal(err)
	}
	var marked []string
	for _, it := range page.Activities {
		if it.Unread {
			id := it.ActivityType + "@" + it.Time
			if it.Notice != nil {
				id = it.Notice.ID
			}
			marked = append(marked, id)
		}
	}
	count, err := UnreadActivityCount(context.Background(), me, "0x1")
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(marked)) != count || len(marked) != 5 {
		t.Errorf("the Inbox marks %d items unread %v and the badge counts %d; want the same 5 (C8)", len(marked), marked, count)
	}
	if want := fmt.Sprint([]string{"MENTION@" + at(25*time.Minute), "n-newer", "n-new", "REACTION@" + at(10*time.Minute), "n-since"}); fmt.Sprint(marked) != want {
		t.Errorf("the Inbox marks %v unread; want %v", marked, want)
	}
}

func TestTheBadgeReadsNothingMoreWhenNothingIsNew(t *testing.T) {
	w := inboxWorld()
	w.upper = 0
	w.install(t)
	got, err := UnreadActivityCount(context.Background(), uuid.New(), "0x1")
	if err != nil || got != 0 || w.reads != 0 {
		t.Errorf("with nothing stored since the last look the badge counts %d (%v) after %d reads of the feed; want 0 and none", got, err, w.reads)
	}
}

func TestTheBadgeStopsCountingAtItsCap(t *testing.T) {
	w := &badgeWorld{upper: 500}
	for i := range 150 {
		w.feed = append(w.feed, inChannel("unread", time.Duration(300-i)*time.Second))
		w.notices = append(w.notices, noticeAt(fmt.Sprint("n", i), time.Duration(300-i)*time.Second))
	}
	w.install(t)
	got, err := UnreadActivityCount(context.Background(), uuid.New(), "0x1")
	if err != nil || got != unreadActivityCap {
		t.Errorf("300 unread items count %d (%v); want the cap, %d (99+)", got, err, unreadActivityCap)
	}
}

// The person's urgent OneCamp AI nudges count by the same rule (V-577): one
// raised since the last look is unread, one raised before isn't, whatever
// the stored count says, as nudges aren't in it. Only their own are asked for.
func TestTheBadgeCountsUrgentNudgesByTheOneRule(t *testing.T) {
	nudgeAt := func(d time.Duration) dgraphModels.UnifiedActivityItem {
		return dgraphModels.UnifiedActivityItem{ActivityType: "NUDGE", Time: at(d), Priority: PriorityHigh}
	}
	for _, c := range []struct {
		name  string
		upper uint64
		want  uint64
	}{
		{"with nothing stored since the last look", 0, 1},
		{"beside the feed's", 9, 6},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := inboxWorld()
			w.upper = c.upper
			w.install(t)
			me := uuid.New()
			var asked []uuid.UUID
			UrgentNudges = func(_ context.Context, who uuid.UUID, _ int) ([]dgraphModels.UnifiedActivityItem, error) {
				asked = append(asked, who)
				return []dgraphModels.UnifiedActivityItem{nudgeAt(-2 * time.Hour), nudgeAt(5 * time.Minute)}, nil
			}
			got, err := UnreadActivityCount(context.Background(), me, "0x1")
			if err != nil || got != c.want {
				t.Errorf("the badge counts %d (%v); want %d: the nudge raised since the last look, not the one before it", got, err, c.want)
			}
			if len(asked) != 1 || asked[0] != me {
				t.Errorf("the nudges were asked for %v; want the person's own, once", asked)
			}
		})
	}
}

// Without OneCamp AI (the v1 edition, which links no AI package) nothing sets
// the seam, and the badge is the feed's alone.
func TestTheEditionWithoutAICountsNoNudges(t *testing.T) {
	w := inboxWorld()
	w.install(t)
	UrgentNudges = nil
	if got, err := UnreadActivityCount(context.Background(), uuid.New(), "0x1"); err != nil || got != 5 {
		t.Errorf("without AI the badge counts %d (%v); want the feed's 5", got, err)
	}
	w.upper = 0
	if got, err := UnreadActivityCount(context.Background(), uuid.New(), "0x1"); err != nil || got != 0 || w.reads != 1 {
		t.Errorf("without AI and nothing stored since, the badge counts %d (%v) after %d reads of the feed; want 0 and no more reads", got, err, w.reads)
	}
}

// The nudges' list draws its dots by the same rule: MarkUnread marks what the
// badge would count.
func TestNudgesAreMarkedByTheOneRule(t *testing.T) {
	w := inboxWorld()
	w.install(t)
	items := []dgraphModels.UnifiedActivityItem{
		{ActivityType: "NUDGE", Time: at(-time.Hour)},
		{ActivityType: "NUDGE", Time: at(time.Minute)},
	}
	if err := MarkUnread(context.Background(), uuid.New(), items); err != nil {
		t.Fatal(err)
	}
	if items[0].Unread || !items[1].Unread {
		t.Errorf("marked %v, %v; want the one raised since the last look alone", items[0].Unread, items[1].Unread)
	}
}
