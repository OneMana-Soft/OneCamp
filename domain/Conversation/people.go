package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// ErrNoConversation is a DM, group or channel the graph doesn't have.
var ErrNoConversation = errors.New("conversation: the graph has no such conversation")

// PeopleOf is the people of the DM or group groupingID, as the graph has them
// (uid, uuid, names, picture, flags), whoever asks: what a share and a group's
// opening name them by. ErrNoConversation when there is no such conversation.
func PeopleOf(ctx context.Context, groupingID string) ([]*dgraphStruct.DgraphUser, error) {
	got, err := PeopleOfMany(ctx, []string{groupingID})
	if err != nil {
		return nil, err
	}
	people, ok := got[groupingID]
	if !ok {
		return nil, ErrNoConversation
	}
	return people, nil
}

// PeopleOfMany is each of the groups groupingIDs' people as the graph has
// them (as PeopleOf), by grouping id, in one read: a block for each group,
// its id a variable. A group the graph doesn't have has no entry.
func PeopleOfMany(ctx context.Context, groupingIDs []string) (map[string][]*dgraphStruct.DgraphUser, error) {
	out := map[string][]*dgraphStruct.DgraphUser{}
	ids := slices.Compact(slices.Sorted(slices.Values(groupingIDs)))
	if len(ids) == 0 {
		return out, nil
	}
	if dgraphInit.DgraphClient == nil {
		return nil, errors.New("no graph connection")
	}
	vars := make(map[string]string, len(ids))
	params := make([]string, len(ids))
	var blocks strings.Builder
	for i, id := range ids {
		v := fmt.Sprintf("$g%d", i)
		vars[v], params[i] = id, v+": string"
		fmt.Fprintf(&blocks, `
		g%d(func: eq(dm_grouping_id, %s)) {
			dm_grouping_id
			dm_participants {
				uid user_uuid user_name user_full_name user_email_id user_profile_object_key
				is_bot is_external user_deleted_at
			}
		}`, i, v)
	}
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx,
		`query People(`+strings.Join(params, ", ")+`) {`+blocks.String()+`
	}`, vars)
	if err != nil {
		return nil, err
	}
	var parsed map[string][]struct {
		ID     string                     `json:"dm_grouping_id"`
		People []*dgraphStruct.DgraphUser `json:"dm_participants"`
	}
	if err := json.Unmarshal(resp.Json, &parsed); err != nil {
		return nil, err
	}
	for _, groups := range parsed {
		for _, g := range groups {
			if _, found := slices.BinarySearch(ids, g.ID); found {
				out[g.ID] = g.People
			}
		}
	}
	return out, nil
}

// InNow keeps those of ids who are in the DM or group grpID now, in their
// order: a DM's two people, as its id names them (a deactivated one is kept
// here; the senders' own gates, Deliver's and Tell's, drop anyone inactive),
// or of a group's people (PeopleOf; nil for a group the graph doesn't have,
// which keeps nobody) those not deactivated. Pure: the one rule
// business/UserChatNotification InConversationNow and the email worker's
// check at the send read.
func InNow(grpID string, people []*dgraphStruct.DgraphUser, ids []string) []string {
	in := map[string]bool{}
	if helpers.IsDMGroupingID(grpID) {
		for _, p := range strings.Fields(grpID) {
			in[strings.ToLower(p)] = true
		}
	} else {
		for _, p := range people {
			if p != nil && p.Uuid != "" && !helpers.IsSoftDeleted(p.DeletedAt) {
				in[strings.ToLower(p.Uuid)] = true
			}
		}
	}
	var kept []string
	for _, id := range ids {
		if in[strings.ToLower(id)] {
			kept = append(kept, id)
		}
	}
	return kept
}

// ChannelPeople is a channel's members as the graph has them (uid, uuid and
// the flags that say who can sign in), whoever asks: who a board made in the
// channel's call is shared with. ErrNoConversation when there is no such
// channel.
func ChannelPeople(ctx context.Context, channelUUID string) ([]*dgraphStruct.DgraphUser, error) {
	// ch_uuid answers a channel with nobody in it, which isn't a missing one.
	c, err := dgexec.First[struct {
		People []*dgraphStruct.DgraphUser `json:"ch_members"`
	}](ctx, `query ChannelPeople($ch: string) {
		c(func: eq(ch_uuid, $ch)) {
			ch_uuid
			ch_members { uid user_uuid is_bot is_external user_deleted_at }
		} }`, map[string]string{"$ch": channelUUID}, "c")
	if errors.Is(err, dgexec.ErrNotFound) {
		return nil, ErrNoConversation
	}
	if err != nil {
		return nil, err
	}
	return c.People, nil
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
