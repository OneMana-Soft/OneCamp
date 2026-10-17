package domain

import (
	"strings"
	"testing"
	"time"
)

// A channel's count is its mentions after its own read mark, never another
// channel's, and a channel without a mark (one the person isn't in) counts
// nothing.
func TestCountUnreadMentionsHoldsEachChannelToItsOwnMark(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 10, 14, h, 0, 0, 0, time.UTC) }
	in := func(ch string, h int) mentionedPost {
		p := mentionedPost{CreatedAt: at(h)}
		p.Channel = &struct {
			Uuid string `json:"ch_uuid"`
		}{Uuid: ch}
		return p
	}
	marks := map[string]time.Time{"launch": at(9), "general": at(12)}
	got := countUnreadMentions([]mentionedPost{
		in("launch", 10), in("launch", 11), // after #launch's mark: 2
		in("general", 10), // before #general's mark: read
		in("general", 13), // after it: 1
		in("left", 14),    // a channel they aren't in
		{CreatedAt: at(15)},
	}, marks)
	if got["launch"] != 2 || got["general"] != 1 || len(got) != 2 {
		t.Errorf("counts %v, want launch 2 and general 1", got)
	}
}

func TestOldestMarkIsTheEarliest(t *testing.T) {
	early := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	marks := map[string]time.Time{"b": early.Add(48 * time.Hour), "a": early, "c": early.Add(time.Hour)}
	if got := oldestMark(marks); !got.Equal(early) {
		t.Errorf("oldest %v, want %v", got, early)
	}
}

// The graph reads the person's mentions and nothing else: counting each
// channel's posts against its mark would read every post of every channel
// they are in, on every sidebar load.
func TestChannelMentionsQueryStartsFromThePersonsMentions(t *testing.T) {
	if strings.Count(channelMentionsQuery, "~mention_users") != 1 || !strings.Contains(channelMentionsQuery, "posts(func: uid(mentioned))") {
		t.Errorf("the query doesn't start from the person's mentions:\n%s", channelMentionsQuery)
	}
	if strings.Contains(channelMentionsQuery, "ch_posts") {
		t.Errorf("the query reads channels' posts:\n%s", channelMentionsQuery)
	}
	for _, rule := range []string{`not gt(post_deleted_at, "1970-01-01T00:00:00Z")`, "NOT uid_in(post_by, $me)", "gt(post_created_at, $since)"} {
		if !strings.Contains(channelMentionsQuery, rule) {
			t.Errorf("the query lacks %s", rule)
		}
	}
}
