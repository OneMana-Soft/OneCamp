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
		// An event's call: whoever may read the event's details (gcal-15 head B).
		if in, err := memberOfEventCall(ctx, room, &who.UserDgraphInfo); err != nil || in {
			return in, false, err
		}
	}
	d, err := conversationBusiness.Resolve(ctx, who, callConversation(ref))
	if errors.Is(err, conversationBusiness.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, onlyInvited(d.Reasons), nil
}

// Reading is how someone may see the calls of what a call belongs to
// (MayReadAll): Invitee when only by a link to the call going on, so what it
// belongs to stays out of their reach.
type Reading struct {
	Invitee bool
}

// MayReadAll is MayRead for the calls of many conversations at once, in as
// few reads as the Access Resolver can (conversationBusiness.ResolveAll): the
// refs whose calls who may see, each with how; one they may not is left out.
// An instant meeting is left out too: its host's rule is MayRead's, one read
// each.
func MayReadAll(ctx context.Context, who *userModels.UserInfo, refs []Ref) (map[Ref]Reading, error) {
	out := make(map[Ref]Reading, len(refs))
	asked := make([]conversationBusiness.Ref, 0, len(refs))
	of := make(map[conversationBusiness.Ref]Ref, len(refs))
	for _, ref := range refs {
		c := callConversation(ref)
		if _, dup := of[c]; dup || ref.Kind == KindMeeting {
			continue
		}
		of[c] = ref
		asked = append(asked, c)
	}
	got, err := conversationBusiness.ResolveAll(ctx, who, asked)
	if err != nil {
		return nil, err
	}
	for c, d := range got {
		if ref, ok := of[c]; ok {
			out[ref] = Reading{Invitee: onlyInvited(d.Reasons)}
		}
	}
	return out, nil
}

// callConversation is what the Access Resolver decides a call's readers by:
// the call kind, at the room of what it belongs to. Pure.
func callConversation(ref Ref) conversationBusiness.Ref {
	return conversationBusiness.Ref{Kind: conversationBusiness.KindCall, ID: RoomOf(ref)}
}

// onlyInvited is a decision that came of a link to the call alone (HU13). Pure.
func onlyInvited(reasons []string) bool {
	return slices.Contains(reasons, conversationBusiness.ReasonCallInvite)
}
