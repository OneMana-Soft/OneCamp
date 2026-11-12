package business

import (
	"context"
	"testing"
)

// A notice from a DM or a group opens only while its reader is in the
// conversation now, and is never named.
func TestAConversationNoticeOpensOnlyWhileTheReaderIsInIt(t *testing.T) {
	readable0 := noticeReadable
	t.Cleanup(func() { noticeReadable = readable0 })
	noticeReadable = func(_ context.Context, user, id string) (bool, error) { return user == "ana" && id == "g1", nil }
	if open, titles, err := inboxNotices(context.Background(), "ana", []string{"g1", "g2"}); err != nil || !open["g1"] || open["g2"] || len(titles) != 0 {
		t.Errorf("ana's conversation notices read open %v, titles %v (%v); want g1 alone, unnamed", open, titles, err)
	}
}
