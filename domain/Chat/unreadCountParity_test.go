package domain

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The two unread-count queries must ask the same question.
//
// A channel's unread count and a conversation's unread count are the same idea
// over different tables, and they were written months apart by the same hand
// with different care. The channel one excluded your own posts, excluded deleted
// posts, and compared strictly after the marker. The chat one did none of those.
//
// The consequence was a badge that could not be cleared by reading, because
// nothing you could do would make the count fall:
//
//   - your own message counted against you, and CreateChat stamps the row with
//     Postgres NOW() AFTER the Go timestamp it stores as your last_seen, so with
//     >= every message you sent registered as one unread to yourself
//   - a deleted message counted forever, and there was nothing left to open
//
// This test pins the parity rather than the text, so either query may be
// rewritten as long as it still answers the question the same way.
func TestUnreadCountQueriesAgree(t *testing.T) {
	chat := readSource(t, "chatDomain.go")
	channel := readSource(t, "../Post/postDomain.go")

	chatQ := extractQuery(t, chat, "GetLatestChatMessageCountByUserID")
	channelQ := extractQuery(t, channel, "GetLatestPostInChannelCountByUserID")

	for _, c := range []struct {
		name string
		chat string
		chnl string
	}{
		{"excludes messages you sent yourself", "created_by <> lsc.user_id", "created_by <> lsc.user_id"},
		{"excludes deleted messages", "deleted_at IS NULL", "deleted_at IS NULL"},
	} {
		if !strings.Contains(chatQ, c.chat) {
			t.Errorf("the chat unread query no longer %s: missing %q", c.name, c.chat)
		}
		if !strings.Contains(channelQ, c.chnl) {
			t.Errorf("the channel unread query no longer %s: missing %q", c.name, c.chnl)
		}
	}

	// Strictly after the marker, never at it. The marker is written at the moment
	// of reading, so a row stamped at that instant is one the reader just saw.
	for name, q := range map[string]string{"chat": chatQ, "channel": channelQ} {
		if strings.Contains(q, ">= lsc.user_last_seen") {
			t.Errorf("the %s unread query uses >= against the last-seen marker; a message stamped at "+
				"the instant of reading is one the reader has seen, and >= counts it as unread forever", name)
		}
		if !strings.Contains(q, "> lsc.user_last_seen") {
			t.Errorf("the %s unread query no longer compares against the last-seen marker at all", name)
		}
	}
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("could not read %s: %v", name, err)
	}
	return string(b)
}

// extractQuery pulls the backtick-quoted SQL out of the named function.
func extractQuery(t *testing.T, src, fn string) string {
	t.Helper()
	i := strings.Index(src, "func "+fn)
	if i < 0 {
		t.Fatalf("function %s not found; if it was renamed, this guard needs the new name", fn)
	}
	m := regexp.MustCompile("(?s)`([^`]*)`").FindStringSubmatch(src[i:])
	if m == nil {
		t.Fatalf("no SQL literal found in %s", fn)
	}
	return m[1]
}
