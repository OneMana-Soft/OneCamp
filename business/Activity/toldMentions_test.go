package business

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	domain "github.com/akashc777/OneCamp/domain/Activity"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

// A mention the reader was told of by a notice (an edit named them) leaves the
// page: the notice lists it, at the edit. Every other item stays, and nothing
// is read for a page without mentions.
func TestAMentionToldByANoticeIsListedOnceAsTheNotice(t *testing.T) {
	const me = "11111111-1111-4111-8111-111111111111"
	reads := stubPageSettings(t, domain.PageSettings{Held: map[string]bool{"mention|post|p1|" + me: true, "mention|comment|c1|" + me: true}}, nil)
	page := []dgraphModels.UnifiedActivityItem{
		{ActivityType: "MENTION", Mention: &dgraphStruct.DgraphMentions{Post: &dgraphStruct.DgraphPost{Uuid: "p1"}}},
		{ActivityType: "MENTION", Mention: &dgraphStruct.DgraphMentions{Post: &dgraphStruct.DgraphPost{Uuid: "p2"}}},
		{ActivityType: "MENTION", Mention: &dgraphStruct.DgraphMentions{Comment: &dgraphStruct.DgraphComment{Uuid: "c1"}}},
		{ActivityType: "MENTION", Mention: &dgraphStruct.DgraphMentions{Chat: &dgraphStruct.DgraphChat{Uuid: "m1"}}},
		{ActivityType: "COMMENT", Comment: &dgraphStruct.DgraphComment{Uuid: "c1"}},
	}
	got := toldAndHeard(context.Background(), me, page)
	var left []string
	for _, it := range got {
		left = append(left, it.ActivityType+":"+toldMentionKey(it, me))
	}
	want := []string{"MENTION:mention|post|p2|" + me, "MENTION:mention|chat|m1|" + me, "COMMENT:"}
	if !slices.Equal(left, want) {
		t.Errorf("left %v, want %v", left, want)
	}
	asked := []string{"mention|post|p1|" + me, "mention|post|p2|" + me, "mention|comment|c1|" + me, "mention|chat|m1|" + me}
	if len(*reads) != 1 || !strings.HasPrefix((*reads)[0], fmt.Sprintf("keys=%v ", asked)) {
		t.Errorf("read %v; want the page's four mentions in one read", *reads)
	}
	*reads = nil
	if got := toldAndHeard(context.Background(), me, page[4:]); len(got) != 1 || len(*reads) != 0 {
		t.Errorf("a page without mentions: %d items, read %v; want it whole and nothing read", len(got), *reads)
	}
	stubPageSettings(t, domain.PageSettings{}, errors.New("down"))
	if got := toldAndHeard(context.Background(), me, page); len(got) != len(page) {
		t.Errorf("a read that failed left %d of %d items; want the page as it was", len(got), len(page))
	}
}
