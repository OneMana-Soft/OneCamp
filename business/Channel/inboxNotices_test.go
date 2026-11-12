package business

import (
	"context"
	"testing"
)

// A notice from a channel (a mention, a run's moment) stays in its reader's
// Inbox after they leave the channel, unopened and unnamed (so without its
// words: NoticeItem): the Inbox asks the channel's own rule as it reads, and
// names only what they may open.
func TestAChannelNoticeOpensOnlyWhileTheChannelIsRead(t *testing.T) {
	readable0, names0 := noticeReadable, noticeNames
	t.Cleanup(func() { noticeReadable, noticeNames = readable0, names0 })
	noticeReadable = func(_ context.Context, user, id string) (bool, error) { return user == "ana" && id == "ch1", nil }
	var named []string
	noticeNames = func(_ context.Context, ids []string) (map[string]string, error) {
		named = append(named, ids...)
		return map[string]string{"ch1": "launch", "ch2": "exec"}, nil
	}
	open, titles, err := inboxNotices(context.Background(), "ana", []string{"ch1", "ch2", "ch1"})
	if err != nil || !open["ch1"] || open["ch2"] || titles["ch1"] != "launch" || len(named) != 1 || named[0] != "ch1" {
		t.Errorf("ana's channel notices read open %v, titles %v, named %v (%v); want #launch alone open and named, #exec not even asked for its name", open, titles, named, err)
	}
	if open, _, err := inboxNotices(context.Background(), "cy", []string{"ch1"}); err != nil || open["ch1"] {
		t.Errorf("cy, out of the channel, reads it open: %v (%v)", open, err)
	}
}
