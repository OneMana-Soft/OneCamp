package business

import (
	"context"
	"errors"
	"slices"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// MayRead answers whether a member may see the calls of what a call belongs
// to: the people who can open its conversation (the Access Resolver's call
// kind), an instant meeting's own host, or someone let into the call going on
// by a link they opened as themselves (HU13). For them, invitee: what the call
// belongs to stays out of their reach. The calls' routes ask it of whoever
// reads, and a meeting's webhooks of their owners (webhooks.go).
func MayRead(ctx context.Context, who *userModels.UserInfo, ref Ref) (allowed, invitee bool, err error) {
	room := RoomOf(ref)
	if ref.Kind == KindMeeting {
		host, err := liveKitBusiness.HostByRule(ctx, room, &who.UserDgraphInfo, false)
		if err != nil || host {
			return host, false, err
		}
	}
	d, err := conversationBusiness.Resolve(ctx, who, conversationBusiness.Ref{Kind: conversationBusiness.KindCall, ID: room})
	if errors.Is(err, conversationBusiness.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, slices.Contains(d.Reasons, conversationBusiness.ReasonCallInvite), nil
}
