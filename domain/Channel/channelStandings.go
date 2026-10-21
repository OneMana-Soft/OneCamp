package domain

import (
	"context"
	"strconv"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// ChannelStandingsBatch bounds the channels one ChannelStandings read takes.
const ChannelStandingsBatch = 500

// ChannelStandings reads, for many channels in one query, what deciding one
// person's access takes (business/Conversation channelDecision): whether
// they're a member or a moderator, whether the channel is private, its
// posting policy and whether it's archived. The same fields
// GetBasicDgraphChannelInfoByUUID reads for one channel, which a list of
// channels used to read one query each. At most ChannelStandingsBatch ids;
// one that isn't a uuid, or a channel that isn't there, is left out.
func ChannelStandings(ctx context.Context, channelUUIDs []string, userDgraphUID string) ([]*dgraphStruct.DgraphChannel, error) {
	list := dgraphquery.UUIDList(channelUUIDs)
	if list == "" {
		return nil, nil
	}
	out, err := dgexec.List[*dgraphStruct.DgraphChannel](ctx, `query standings($userId: string) {
		c(func: eq(ch_uuid, [`+list+`]), first: `+strconv.Itoa(ChannelStandingsBatch)+`) {
			uid
			ch_uuid
			ch_name
			ch_is_member: count(ch_members @filter(uid($userId)))
			ch_is_admin: count(ch_moderators @filter(uid($userId)))
			ch_private
			ch_post_policy
			ch_deleted_at
		}
	}`, map[string]string{"$userId": dgraphUIDOrNone(userDgraphUID)}, "c")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ChannelStandings err: %+v", err)
	}
	return out, err
}
