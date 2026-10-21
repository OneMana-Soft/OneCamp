package domain

// A thread's replies, read from the graph for several threads at once (chat
// P2, business/ChatThread): how many live replies each root has, and its
// latest few, newest first. A channel thread's replies are its post's
// comments, a DM's or group's its message's.

import (
	"context"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
)

// Reply is one reply in a thread.
type Reply struct {
	ID        string     `json:"id"`
	HTML      string     `json:"html"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	By        *Person    `json:"by,omitempty"`
}

// Replies is a thread's live replies: how many, and the latest, newest first.
type Replies struct {
	Count  int
	Latest []Reply
}

// MaxLatest is the most replies a thread's read takes.
const MaxLatest = 20

// RepliesOf reads the threads roots name: posts' when channel, else a DM's or
// group's messages'. latest is how many of each one's newest replies to take
// (at most MaxLatest). A root that isn't there is missing from the answer.
func RepliesOf(ctx context.Context, channel bool, roots []string, latest int) (map[string]Replies, error) {
	list := dgraphquery.UUIDList(roots)
	if list == "" {
		return map[string]Replies{}, nil
	}
	if latest < 1 {
		latest = 1
	}
	if latest > MaxLatest {
		latest = MaxLatest
	}
	pred, idPred, typ := "post_comments", "post_uuid", "Post"
	if !channel {
		pred, idPred, typ = "chat_comments", "chat_uuid", "Chat"
	}
	live := "@filter(" + dgraphquery.Live("comment_deleted_at") + ")"
	q := fmt.Sprintf(`{
		t(func: eq(%[2]s, [%[1]s]), first: %[6]d) @filter(type(%[3]s)) {
			id: %[2]s
			n: count(%[4]s %[5]s)
			latest: %[4]s(orderdesc: comment_created_at, first: %[7]d) %[5]s {
				id: comment_uuid
				html: comment_text
				created_at: comment_created_at
				by: comment_by {
					user_uuid
					user_name
					user_full_name
					user_profile_object_key
					is_bot
				}
			}
		}
	}`, list, idPred, typ, pred, live, MaxRead, latest)
	var out struct {
		T []struct {
			ID     string  `json:"id"`
			N      int     `json:"n"`
			Latest []Reply `json:"latest"`
		} `json:"t"`
	}
	if err := dgraphInit.ReadInto(ctx, q, nil, &out); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/Message RepliesOf err: %+v", err)
		return nil, err
	}
	got := make(map[string]Replies, len(out.T))
	for _, t := range out.T {
		got[t.ID] = Replies{Count: t.N, Latest: t.Latest}
	}
	return got, nil
}
