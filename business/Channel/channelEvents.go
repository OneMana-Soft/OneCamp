package business

// A channel's changes, told to the Resource Events bus by the person who made
// them (chat B28): who joined, left, was added or removed; a rename, a new
// topic, a privacy change, an archive and a restore. Their sinks write the
// channel's history lines (business/Conversation channelLines.go), the audit
// log and webhooks. Emitted only where a person acts (the channel routes),
// never by a seed, an import or a drill.

import (
	"context"
	"strconv"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Channel"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// emit is the bus: a seam for tests.
var emit = func(ctx context.Context, e resourceEventBusiness.Event) { resourceEventBusiness.Bus.Emit(ctx, e) }

// EmitMembership tells the bus actor added (or, with added false, removed)
// members of the channel; an actor naming only themself joined (left).
func EmitMembership(ctx context.Context, added bool, channelUUID, actorUUID string, members ...string) {
	if channelUUID == "" || actorUUID == "" || len(members) == 0 {
		return
	}
	kind := resourceEventBusiness.MemberRemoved
	if added {
		kind = resourceEventBusiness.MemberAdded
	}
	emit(ctx, resourceEventBusiness.Event{Kind: kind, ResourceKind: "channel", ResourceUUID: channelUUID,
		Actor: resourceEventBusiness.Actor{UserUUID: actorUUID}, At: time.Now(), Subjects: members})
}

// ChannelChanges is what an update changed, from the channel as it was and the
// update as it was saved. Pure.
func ChannelChanges(before *dgraphStruct.DgraphChannel, after *adapter.UpdateChannelInfo, privacy PrivacyChange) []resourceEventBusiness.Event {
	if before == nil || after == nil {
		return nil
	}
	var out []resourceEventBusiness.Event
	add := func(kind resourceEventBusiness.Kind, data map[string]string) {
		out = append(out, resourceEventBusiness.Event{Kind: kind, ResourceKind: "channel", ResourceUUID: before.Uuid, Data: data})
	}
	archived := helpers.IsSoftDeleted(before.DeletedAt)
	switch {
	case after.ChannelArchived && !archived:
		add(resourceEventBusiness.ChannelArchived, nil)
		return out // an archive changes nothing else people read
	case !after.ChannelArchived && archived:
		add(resourceEventBusiness.ChannelRestored, nil)
	}
	if after.ChannelName != "" && after.ChannelName != before.Name {
		add(resourceEventBusiness.ChannelRenamed, map[string]string{"name": after.ChannelName, "was": before.Name})
	}
	if after.ChannelAbout != "" && after.ChannelAbout != before.About {
		add(resourceEventBusiness.ChannelTopicChanged, map[string]string{"topic": after.ChannelAbout})
	}
	if privacy == PrivacyOpened || privacy == PrivacyClosed {
		add(resourceEventBusiness.ChannelPrivacyChanged, map[string]string{"private": strconv.FormatBool(privacy == PrivacyClosed)})
	}
	return out
}

// EmitChannelChanges tells the bus what actor's update changed.
func EmitChannelChanges(ctx context.Context, actorUUID string, before *dgraphStruct.DgraphChannel, after *adapter.UpdateChannelInfo, privacy PrivacyChange) {
	now := time.Now()
	for _, e := range ChannelChanges(before, after, privacy) {
		e.Actor, e.At = resourceEventBusiness.Actor{UserUUID: actorUUID}, now
		emit(ctx, e)
	}
}
