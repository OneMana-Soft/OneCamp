package domain

// A thread reply sent to its channel too ("Also send to #channel") is two
// messages: the reply, and a copy of it posted in the channel. The copy holds
// an edge to its reply (post_broadcast_of, the predicate the chat programme
// reserves for it, PROGRAM §3.8), read from the reply as ~post_broadcast_of,
// so that an edit or a delete of the reply reaches the copy: they used to
// reach the reply only, and a deleted reply stayed readable in the channel as
// its copy.

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// LinkReplyCopy records that the post with copyUUID is the copy of the reply
// with commentUUID. Nothing is written unless both are found.
func LinkReplyCopy(ctx context.Context, commentUUID, copyUUID string) error {
	_, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query: `query q($c: string, $p: string) {
			co as var(func: eq(comment_uuid, $c))
			po as var(func: eq(post_uuid, $p))
		}`,
		Vars:      map[string]string{"$c": commentUUID, "$p": copyUUID},
		Mutations: []*api.Mutation{{SetNquads: []byte(`uid(po) <post_broadcast_of> uid(co) .`), Cond: "@if(eq(len(co), 1) AND eq(len(po), 1))"}},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/LinkReplyCopy err: %+v", err)
	}
	return err
}

// GetReplyCopy is the copy of the reply with commentUUID posted in its channel,
// with its author, channel and deletion time, or nil when it has none.
func GetReplyCopy(ctx context.Context, commentUUID string) (*dgraphStruct.DgraphPost, error) {
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, `query q($c: string) {
		q(func: eq(comment_uuid, $c)) {
			copy: ~post_broadcast_of (first: 1) {
				uid
				post_uuid
				post_deleted_at
				post_by { uid user_uuid }
				post_channel { uid ch_uuid }
			}
		}
	}`, map[string]string{"$c": commentUUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetReplyCopy err: %+v", err)
		return nil, err
	}
	var out struct {
		Q []struct {
			Copy []*dgraphStruct.DgraphPost `json:"copy"`
		} `json:"q"`
	}
	if err := json.Unmarshal(resp.GetJson(), &out); err != nil {
		return nil, err
	}
	if len(out.Q) == 0 || len(out.Q[0].Copy) == 0 {
		return nil, nil
	}
	return out.Q[0].Copy[0], nil
}
