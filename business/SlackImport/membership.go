package business

import (
	"context"
	"sync"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/google/uuid"
)

// dgraphUserCache memoises user-uuid → DgraphUser lookups across all
// channels in a single import. Channels typically share many members,
// so this saves N-channel × M-member round trips down to one lookup
// per unique user.
type dgraphUserCache struct {
	mu sync.Mutex
	m  map[string]*dgraphStruct.DgraphUser
}

func newDgraphUserCache() *dgraphUserCache {
	return &dgraphUserCache{m: make(map[string]*dgraphStruct.DgraphUser, 64)}
}

func (c *dgraphUserCache) get(ctx context.Context, userUUID string) *dgraphStruct.DgraphUser {
	c.mu.Lock()
	if u, ok := c.m[userUUID]; ok {
		c.mu.Unlock()
		return u
	}
	c.mu.Unlock()

	u, err := userBusiness.GetDgraphUserInfoByUUID(ctx, userUUID)
	if err != nil || u == nil {
		return nil
	}
	c.mu.Lock()
	c.m[userUUID] = u
	c.mu.Unlock()
	return u
}

// addChannelMembersBulk wires up channel membership for an imported
// channel. The implementation calls the existing AddChannelMemberEdge
// per member because that path also creates the LastSeenChannel row
// and the per-user notification preference — both of which are needed
// for imported users to see the channel correctly.
//
// AddChannelMemberEdge skips its `user.joined` webhook dispatch when
// helpers.IsBulkImport(ctx) is true, so callers MUST wrap ctx with
// BulkImportContextKey=true (the orchestrator does this once at the
// top of the run) to avoid storming the operator's outbound webhooks
// with thousands of historical-membership events.
//
// Caller is responsible for wrapping ctx with BulkImportContextKey=true
// so downstream MQTT/activity emitters skip their per-member side-effect.
func addChannelMembersBulk(ctx context.Context, importId uuid.UUID, channelUUID uuid.UUID,
	slackMemberIds []string, cache *dgraphUserCache) {

	if len(slackMemberIds) == 0 {
		return
	}

	idMap, err := importModels.LookupIdMappingsBatch(ctx, importId, importModels.EntityUser, slackMemberIds)
	if err != nil {
		helpers.LogWarnWithContext(ctx,
			"SlackImport.addChannelMembersBulk lookup failed channel=%s err=%+v",
			channelUUID, err)
		return
	}

	added := 0
	for _, slackUserId := range slackMemberIds {
		ocUUID, ok := idMap[slackUserId]
		if !ok {
			// User wasn't resolved (e.g., USLACKBOT or a deleted account
			// without an entry in users.json). Skip silently.
			continue
		}
		dgUser := cache.get(ctx, ocUUID.String())
		if dgUser == nil {
			continue
		}
		if err := channelBusiness.AddChannelMemberEdge(ctx, channelUUID, dgUser, ocUUID); err != nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport.addChannelMembersBulk add failed channel=%s user=%s err=%+v",
				channelUUID, ocUUID, err)
			continue
		}
		added++
	}
	helpers.LogInfoWithContext(ctx,
		"SlackImport channel %s: added %d/%d members", channelUUID, added, len(slackMemberIds))
}
