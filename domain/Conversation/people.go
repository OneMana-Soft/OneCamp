package domain

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// ErrNoConversation is a DM, group or channel the graph doesn't have.
var ErrNoConversation = errors.New("conversation: the graph has no such conversation")

// PeopleOf is the people of the DM or group groupingID, as the graph has them
// (uid, uuid, names, picture, flags), whoever asks: what a share and a group's
// opening name them by. ErrNoConversation when there is no such conversation.
func PeopleOf(ctx context.Context, groupingID string) ([]*dgraphStruct.DgraphUser, error) {
	if dgraphInit.DgraphClient == nil {
		return nil, errors.New("no graph connection")
	}
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, `query People($grp: string) {
		g(func: eq(dm_grouping_id, $grp)) {
			dm_grouping_id
			dm_participants {
				uid user_uuid user_name user_full_name user_email_id user_profile_object_key
				is_bot is_external user_deleted_at
			}
		} }`, map[string]string{"$grp": groupingID})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		G []struct {
			ID     string                     `json:"dm_grouping_id"`
			People []*dgraphStruct.DgraphUser `json:"dm_participants"`
		} `json:"g"`
	}
	if err := json.Unmarshal(resp.Json, &parsed); err != nil {
		return nil, err
	}
	for _, g := range parsed.G {
		if g.ID == groupingID {
			return g.People, nil
		}
	}
	return nil, ErrNoConversation
}

// ChannelNode is a channel's graph node and name. ErrNoConversation when there
// is no such channel.
func ChannelNode(ctx context.Context, channelUUID string) (*dgraphStruct.DgraphChannel, error) {
	if dgraphInit.DgraphClient == nil {
		return nil, errors.New("no graph connection")
	}
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, `query Channel($ch: string) {
		c(func: eq(ch_uuid, $ch)) { uid ch_uuid ch_name }
	}`, map[string]string{"$ch": channelUUID})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		C []*dgraphStruct.DgraphChannel `json:"c"`
	}
	if err := json.Unmarshal(resp.Json, &parsed); err != nil {
		return nil, err
	}
	for _, c := range parsed.C {
		if c.Uuid == channelUUID && c.Uid != "" {
			return c, nil
		}
	}
	return nil, ErrNoConversation
}
