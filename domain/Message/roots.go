package domain

// Messages with replies, read by when they were written (chat P2's
// chat.threads.adopt sweep, business/ChatThread): each one's conversation,
// author and the writers of its newest live replies. Posts' and messages'
// creation times are indexed by the hour, so a window reads only its own.

import (
	"context"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
)

// ThreadRoot is a live message with live replies.
type ThreadRoot struct {
	ID        string
	In        string // a channel's uuid, a DM's or group's grouping id
	By        string
	CreatedAt time.Time
	// Repliers wrote its newest RootReplies live replies, each once, newest
	// first; LastReply is when the newest was written.
	Repliers  []string
	LastReply time.Time
}

// RootReplies is how many of a root's newest replies say who replied.
const RootReplies = 50

// RootsSince reads up to first live messages with live replies written at or
// after since, oldest first: posts when channel, else DMs' and groups'
// messages. first is at most MaxRead.
func RootsSince(ctx context.Context, channel bool, since time.Time, first int) ([]ThreadRoot, error) {
	if first < 1 || first > MaxRead {
		first = MaxRead
	}
	at, typ, id, by, in, inID, replies, deleted := "post_created_at", "Post", "post_uuid", "post_by", "post_channel", "ch_uuid", "post_comments", "post_deleted_at"
	if !channel {
		at, typ, id, by, in, inID, replies, deleted = "chat_created_at", "Chat", "chat_uuid", "chat_from", "chat_dm", "dm_grouping_id", "chat_comments", "chat_deleted_at"
	}
	liveReply := "@filter(" + dgraphquery.Live("comment_deleted_at") + ")"
	q := fmt.Sprintf(`query roots($since: string) {
		r(func: ge(%[1]s, $since), orderasc: %[1]s, first: %[9]d) @filter(type(%[2]s) AND has(%[7]s) AND %[8]s) {
			id: %[3]s
			at: %[1]s
			by: %[4]s { id: user_uuid }
			in: %[5]s { id: %[6]s }
			replies: %[7]s(orderdesc: comment_created_at, first: %[10]d) %[11]s {
				at: comment_created_at
				by: comment_by { id: user_uuid }
			}
		}
	}`, at, typ, id, by, in, inID, replies, dgraphquery.Live(deleted), first, RootReplies, liveReply)
	type ref struct {
		ID string `json:"id"`
	}
	var out struct {
		R []struct {
			ID      string     `json:"id"`
			At      *time.Time `json:"at"`
			By      *ref       `json:"by"`
			In      *ref       `json:"in"`
			Replies []struct {
				At *time.Time `json:"at"`
				By *ref       `json:"by"`
			} `json:"replies"`
		} `json:"r"`
	}
	vars := map[string]string{"$since": since.UTC().Format(time.RFC3339Nano)}
	if err := dgraphInit.ReadInto(ctx, q, vars, &out); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/Message RootsSince err: %+v", err)
		return nil, err
	}
	roots := make([]ThreadRoot, 0, len(out.R))
	for _, r := range out.R {
		if r.At == nil || r.In == nil || r.In.ID == "" || len(r.Replies) == 0 {
			continue
		}
		root := ThreadRoot{ID: r.ID, In: r.In.ID, CreatedAt: *r.At}
		if r.By != nil {
			root.By = r.By.ID
		}
		seen := map[string]bool{}
		for _, reply := range r.Replies {
			if reply.At != nil && reply.At.After(root.LastReply) {
				root.LastReply = *reply.At
			}
			if reply.By != nil && reply.By.ID != "" && !seen[reply.By.ID] {
				seen[reply.By.ID] = true
				root.Repliers = append(root.Repliers, reply.By.ID)
			}
		}
		if len(root.Repliers) == 0 && root.LastReply.IsZero() {
			continue
		}
		roots = append(roots, root)
	}
	return roots, nil
}
