package domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// GetChatThreadRepliers is who has replied in the thread of the DM or group
// message chatUUID, by user id (deleted replies aside): the thread's people,
// as GetDgraphPostCommentsInfoByUUID reads a channel thread's.
func GetChatThreadRepliers(ctx context.Context, chatUUID string) ([]string, error) {
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, `query q($id: string) {
		q(func: eq(chat_uuid, $id)) {
			chat_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")) {
				comment_by { user_uuid }
			}
		}
	}`, map[string]string{"$id": chatUUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetChatThreadRepliers err: %+v", err)
		return nil, err
	}
	var out struct {
		Q []struct {
			Comments []struct {
				By *struct {
					Uuid string `json:"user_uuid"`
				} `json:"comment_by"`
			} `json:"chat_comments"`
		} `json:"q"`
	}
	if err := json.Unmarshal(resp.GetJson(), &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, q := range out.Q {
		for _, c := range q.Comments {
			if c.By != nil && c.By.Uuid != "" {
				ids = append(ids, c.By.Uuid)
			}
		}
	}
	return ids, nil
}

// LastRepliesBy is when the person (their node, me) last replied in the thread
// of each of posts (channel messages) and of chats (DM or group messages), by
// the message's uuid, deleted replies aside, in one read. A thread they
// haven't replied in has no entry.
func LastRepliesBy(ctx context.Context, me string, posts, chats []string) (map[string]time.Time, error) {
	at := map[string]time.Time{}
	postList, chatList := dgraphquery.UUIDList(posts), dgraphquery.UUIDList(chats)
	if me == "" || (postList == "" && chatList == "") {
		return at, nil
	}
	theirs := `(orderdesc: comment_created_at, first: 1)
			@filter(uid_in(comment_by, $me) AND ` + dgraphquery.Live("comment_deleted_at") + `) { comment_created_at }`
	q := `query q($me: string) {`
	if postList != "" {
		q += `
		p(func: eq(post_uuid, [` + postList + `])) { id: post_uuid replies: post_comments ` + theirs + ` }`
	}
	if chatList != "" {
		q += `
		c(func: eq(chat_uuid, [` + chatList + `])) { id: chat_uuid replies: chat_comments ` + theirs + ` }`
	}
	type thread struct {
		ID      string `json:"id"`
		Replies []struct {
			At time.Time `json:"comment_created_at"`
		} `json:"replies"`
	}
	got, err := dgexec.Read[struct {
		P []thread `json:"p"`
		C []thread `json:"c"`
	}](ctx, q+"\n\t}", map[string]string{"$me": me})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/LastRepliesBy err: %+v", err)
		return at, err
	}
	for _, t := range append(got.P, got.C...) {
		if len(t.Replies) > 0 {
			at[t.ID] = t.Replies[0].At
		}
	}
	return at, nil
}
