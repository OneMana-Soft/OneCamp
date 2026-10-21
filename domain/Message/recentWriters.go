package domain

// Who wrote in a conversation lately, for the "@" list's order (chat P3,
// D-M9: this channel's people by recent activity).

import (
	"context"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
)

// recentRead is how many of a conversation's newest messages are read for
// who wrote them.
const recentRead = 100

// RecentWriters is the people who wrote the newest messages in a channel
// (kind "channel", its uuid) or a DM or group (its grouping id), each once,
// the most recent first, up to n.
func RecentWriters(ctx context.Context, kind, id string, n int) ([]string, error) {
	var q string
	switch kind {
	case "channel":
		q = `query Recent($id: string){
			c(func: eq(ch_uuid, $id)) {
				m: ch_posts @filter(` + dgraphquery.Live("post_deleted_at") + `) (orderdesc: post_created_at, first: ` + fmt.Sprint(recentRead) + `) {
					by: post_by { user_uuid }
				}
			}
		}`
	case "dm", "group":
		q = `query Recent($id: string){
			c(func: eq(dm_grouping_id, $id)) {
				m: dm_chats @filter(` + dgraphquery.Live("chat_deleted_at") + `) (orderdesc: chat_created_at, first: ` + fmt.Sprint(recentRead) + `) {
					by: chat_from { user_uuid }
				}
			}
		}`
	default:
		return nil, nil
	}
	var out recentAnswer
	if err := dgraphInit.ReadInto(ctx, q, map[string]string{"$id": id}, &out); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/Message RecentWriters err: %+v", err)
		return nil, err
	}
	return out.writers(n), nil
}

// recentAnswer is RecentWriters' query answered: a conversation's newest
// messages and who wrote each.
type recentAnswer struct {
	C []struct {
		M []struct {
			By *struct {
				UUID string `json:"user_uuid"`
			} `json:"by"`
		} `json:"m"`
	} `json:"c"`
}

// writers is who wrote the messages, newest first, each once, up to n. Pure.
func (a recentAnswer) writers(n int) []string {
	seen := map[string]bool{}
	var ids []string
	for _, conv := range a.C {
		for _, m := range conv.M {
			if m.By == nil || m.By.UUID == "" || seen[m.By.UUID] {
				continue
			}
			seen[m.By.UUID] = true
			ids = append(ids, m.By.UUID)
			if len(ids) == n {
				return ids
			}
		}
	}
	return ids
}
