package business

import (
	"context"

	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
)

// MarkGuestAccess sets HasGuestAccess on each channel that people outside the
// workspace can read now, for the marker its members see in the channel's
// header and in the channel list: someone holds a live guest link to it (not
// revoked, not expired), guest access is on for the workspace, and the
// channel is neither archived nor deleted. Those are the conditions a guest's
// link opens the channel on (business/Guest ValidateResourceGrant and
// liveChannel), so the marker shows exactly while a guest can read it.
//
// A guest is a link, never a member, so "has guests" and "has a live link"
// are one question: there is no roster of guests to count.
//
// One query for any number of channels, none while guest access is off. A
// marker, not a gate: a read that fails leaves the channels unmarked, is
// logged, and the read it decorates still answers.
func MarkGuestAccess(ctx context.Context, channels ...*dgraphStruct.DgraphChannel) {
	if len(channels) == 0 || !settingsBusiness.GuestAccessEnabled() {
		return
	}
	ids := make([]string, 0, len(channels))
	for _, ch := range channels {
		if ch != nil && ch.Uuid != "" {
			ids = append(ids, ch.Uuid)
		}
	}
	live, err := guestModel.WithActiveGrants(ctx, guestModel.ResourceChannel, ids)
	if err != nil {
		helpers.LogWarnWithContext(ctx, "business/Channel/MarkGuestAccess: reading the channels' guest links: %v", err)
		return
	}
	markGuestAccess(channels, live)
}

// markGuestAccess marks each channel with a live link that is itself live
// (an archived or deleted channel's links open nothing). Pure.
func markGuestAccess(channels []*dgraphStruct.DgraphChannel, linked map[string]bool) {
	for _, ch := range channels {
		if ch == nil {
			continue
		}
		ch.HasGuestAccess = ch.Uuid != "" && linked[ch.Uuid] && !helpers.IsSoftDeleted(ch.DeletedAt)
	}
}
