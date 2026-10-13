package domain

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
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
