package business

// A4 H5: Priority is triage the person can clear. An ask lets go of Priority
// a week after it was asked, and the person can take one out themselves
// (POST /activity/dismiss) and put it back. Either way it reads normal, never
// low: the Inbox reads a counted item that drops to low as read, so low would
// clear its dot in All too. Nothing else changes: the item stays in the feed.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

func TestClearFromPriority(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339Nano) }
	week, day := 7*24*time.Hour, 24*time.Hour
	for _, c := range []struct {
		name          string
		priority      string
		time          string
		dismissed     bool
		want          string
		wantDismissed bool
	}{
		{"an ask a minute short of a week stays", PriorityHigh, at(week - time.Minute), false, PriorityHigh, false},
		{"an ask a minute over a week lets go, to normal", PriorityHigh, at(week + time.Minute), false, PriorityNormal, false},
		{"a dismissed ask reads normal, never low", PriorityHigh, at(time.Hour), true, PriorityNormal, true},
		{"a dismissed ask over a week old has let go anyway: putting it back brings nothing back", PriorityHigh, at(8 * day), true, PriorityNormal, false},
		{"an answered ask stays low", PriorityLow, at(time.Hour), true, PriorityLow, false},
		{"a normal item stays normal", PriorityNormal, at(30 * day), true, PriorityNormal, false},
		{"an ask whose time can't be read stays", PriorityHigh, "not a time", false, PriorityHigh, false},
	} {
		items := []dgraphModels.UnifiedActivityItem{{ActivityType: "MENTION", Key: "MENTION|id|0x1", Priority: c.priority, Time: c.time}}
		clearFromPriority(items, map[string]bool{"MENTION|id|0x1": c.dismissed, "MENTION|id|0x2": true}, now)
		if items[0].Priority != c.want || items[0].Dismissed != c.wantDismissed {
			t.Errorf("%s: reads %q, dismissed %v; want %q, dismissed %v", c.name, items[0].Priority, items[0].Dismissed, c.want, c.wantDismissed)
		}
	}

	// A store that can't be read dismisses nothing (logged), and the week,
	// which reads no store, still applies: an 8-day ask reads normal, a 6-day
	// one stays Priority although its key was dismissed.
	prev := readDismissals
	t.Cleanup(func() { readDismissals = prev })
	readDismissals = func(context.Context, uuid.UUID, []string) (map[string]bool, error) {
		return map[string]bool{"MENTION|id|0x3": true}, errors.New("postgres is down")
	}
	items := []dgraphModels.UnifiedActivityItem{
		{ActivityType: "MENTION", Key: "MENTION|id|0x3", Priority: PriorityHigh, Time: at(6 * day)},
		{ActivityType: "COMMENT", Key: "COMMENT|id|c", Priority: PriorityHigh, Time: at(8 * day)},
	}
	clearFromPriority(items, dismissalsOf(context.Background(), uuid.New(), items), now)
	if items[0].Priority != PriorityHigh || items[0].Dismissed || items[1].Priority != PriorityNormal {
		t.Errorf("with the store down: the 6-day ask %q (dismissed %v), the 8-day %q; want %q, not dismissed, and %q",
			items[0].Priority, items[0].Dismissed, items[1].Priority, PriorityHigh, PriorityNormal)
	}
}

// Every key the feed sends is one POST /activity/dismiss takes; a reaction,
// which is never Priority, has none.
func TestEveryKeyTheFeedSendsCanBeDismissed(t *testing.T) {
	comment, notice := uuid.NewString(), uuid.NewString()
	for _, c := range []struct {
		item dgraphModels.UnifiedActivityItem
		want string
	}{
		{dgraphModels.UnifiedActivityItem{ActivityType: "MENTION", Mention: &dgraphStruct.DgraphMentions{Uid: "0x2a"}}, "MENTION|id|0x2a"},
		{dgraphModels.UnifiedActivityItem{ActivityType: "COMMENT", Comment: &dgraphStruct.DgraphComment{Uid: "0x9", Uuid: comment}}, "COMMENT|id|" + comment},
		{dgraphModels.UnifiedActivityItem{ActivityType: "NOTICE", Notice: &dgraphModels.NoticeItem{ID: notice}}, "NOTICE|id|" + notice},
		{dgraphModels.UnifiedActivityItem{ActivityType: "REACTION", Reaction: &dgraphModels.ReactionsActivity{}}, ""},
		{dgraphModels.UnifiedActivityItem{ActivityType: "MENTION"}, ""},
		{dgraphModels.UnifiedActivityItem{ActivityType: "COMMENT", Comment: &dgraphStruct.DgraphComment{Uid: "0x9"}}, ""},
	} {
		got := itemKey(&c.item)
		if got != c.want {
			t.Errorf("%s's key is %q; want %q", c.item.ActivityType, got, c.want)
		}
		if got != "" && checkKeys([]string{got}) != nil {
			t.Errorf("the feed's key %q is refused by POST /activity/dismiss", got)
		}
	}
}

// Dismiss takes only Priority items' keys, a page of them at most, for the
// person it is given, and writes nothing for a request it refuses.
func TestADismissalTakesOnlyPriorityKeys(t *testing.T) {
	prev := markDismissed
	t.Cleanup(func() { markDismissed = prev })
	var wrote []string
	markDismissed = func(_ context.Context, _ uuid.UUID, keys []string, _ bool) error {
		wrote = append(wrote, keys...)
		return nil
	}
	page := make([]string, 21)
	for i := range page {
		page[i] = "MENTION|id|0x" + strings.Repeat("1", i+1)
	}
	for _, keys := range [][]string{
		nil,
		page,
		{"REACTION|id|0x1"},
		{"MENTION|id|"},
		{""},
		{"MENTION|id|" + strings.Repeat("a", 190)},
		{"MENTION|id|a\x00b"},
		{"MENTION|id|\xff"},
		{"mention|id|0x1"},
		{"MENTION|0x1"},
		{"MENTION|id|0x1", "REACTION|id|0x2"},
	} {
		if err := Dismiss(context.Background(), uuid.New(), keys, true); !errors.Is(err, ErrBadKey) {
			t.Errorf("keys %q: %v; want ErrBadKey", keys, err)
		}
	}
	if len(wrote) != 0 {
		t.Fatalf("refused requests wrote %q", wrote)
	}

	me := uuid.New()
	markDismissed = func(_ context.Context, user uuid.UUID, keys []string, dismissed bool) error {
		if user != me || !dismissed {
			t.Errorf("wrote for %s, dismissed %v; want %s, dismissed", user, dismissed, me)
		}
		wrote = append(wrote, keys...)
		return nil
	}
	ok := []string{"MENTION|id|0x2a", "COMMENT|id|" + uuid.NewString(), "NOTICE|id|" + uuid.NewString()}
	if err := Dismiss(context.Background(), me, append(ok, page[:17]...), true); err != nil {
		t.Fatalf("a page of Priority keys: %v", err)
	}
	if len(wrote) != 20 {
		t.Errorf("wrote %d keys; want the 20 given", len(wrote))
	}
}
