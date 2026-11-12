package business

import (
	"context"
	"errors"
	"slices"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

// A mention the reader was told of by a notice (an edit named them) leaves the
// page: the notice lists it, at the edit. Every other item stays, and nothing
// is read for a page without mentions.
func TestAMentionToldByANoticeIsListedOnceAsTheNotice(t *testing.T) {
	const me = "11111111-1111-4111-8111-111111111111"
	old := heldNotices
	t.Cleanup(func() { heldNotices = old })
	var asked []string
	var fail error
	heldNotices = func(_ context.Context, keys []string) (map[string]bool, error) {
		asked = keys
		return map[string]bool{"mention|post|p1|" + me: true, "mention|comment|c1|" + me: true}, fail
	}
	page := []dgraphModels.UnifiedActivityItem{
		{ActivityType: "MENTION", Mention: &dgraphStruct.DgraphMentions{Post: &dgraphStruct.DgraphPost{Uuid: "p1"}}},
		{ActivityType: "MENTION", Mention: &dgraphStruct.DgraphMentions{Post: &dgraphStruct.DgraphPost{Uuid: "p2"}}},
		{ActivityType: "MENTION", Mention: &dgraphStruct.DgraphMentions{Comment: &dgraphStruct.DgraphComment{Uuid: "c1"}}},
		{ActivityType: "MENTION", Mention: &dgraphStruct.DgraphMentions{Chat: &dgraphStruct.DgraphChat{Uuid: "m1"}}},
		{ActivityType: "COMMENT", Comment: &dgraphStruct.DgraphComment{Uuid: "c1"}},
	}
	got := dropToldMentions(context.Background(), me, page)
	var left []string
	for _, it := range got {
		left = append(left, it.ActivityType+":"+toldMentionKey(it, me))
	}
	want := []string{"MENTION:mention|post|p2|" + me, "MENTION:mention|chat|m1|" + me, "COMMENT:"}
	if !slices.Equal(left, want) {
		t.Errorf("left %v, want %v", left, want)
	}
	if len(asked) != 4 {
		t.Errorf("asked about %v; want the page's four mentions in one read", asked)
	}

	asked = nil
	if got := dropToldMentions(context.Background(), me, page[4:]); len(got) != 1 || asked != nil {
		t.Errorf("a page without mentions: %d items, asked %v; want it whole and nothing read", len(got), asked)
	}
	fail = errors.New("down")
	if got := dropToldMentions(context.Background(), me, page); len(got) != len(page) {
		t.Errorf("a read that failed left %d of %d items; want the page as it was", len(got), len(page))
	}
}
