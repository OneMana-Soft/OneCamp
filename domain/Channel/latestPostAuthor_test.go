package domain

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestChannelListLatestPostNamesItsAuthor: the channel list shows each
// channel's latest message as "Author: text". For a message a channel guest
// or a Slack person wrote, the author is the Guests or Slack bot and the
// person is named in a label the message starts with; the web app tells
// those apart by the author's uuid and is_bot, and shows "Priya (Acme): text"
// instead of "Guests: [Priya (Acme) (guest)]text". Every latest-post query
// must select them, or that list falls back to the bot's name and the label.
func TestChannelListLatestPostNamesItsAuthor(t *testing.T) {
	src, err := os.ReadFile("channelDomain.go")
	if err != nil {
		t.Fatal(err)
	}
	latest := regexp.MustCompile(`(?s)ch_posts @filter\(not gt\(post_deleted_at, "1970-01-01T00:00:00Z"\)\) \(orderdesc: post_created_at, first: 1\) \{.*?post_by \{(.*?)\}`)
	blocks := latest.FindAllStringSubmatch(string(src), -1)
	if len(blocks) == 0 {
		t.Fatal("no latest-post query found; has the channel list moved?")
	}
	for i, b := range blocks {
		fields := strings.Fields(b[1])
		has := map[string]bool{}
		for _, f := range fields {
			has[f] = true
		}
		for _, want := range []string{"user_uuid", "user_name", "user_full_name", "is_bot"} {
			if !has[want] {
				t.Errorf("latest-post query %d selects post_by %v, without %s", i+1, fields, want)
			}
		}
	}
}
