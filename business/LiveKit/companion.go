package business

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Companions (calls meet-04): another device of someone in a call, joined for
// the chat, the hands and the reactions, or to present a screen, without a
// microphone, a camera or a speaker (livekitInit's companion.go).

// ErrNoCompanionHere: a companion asked for by someone who may not be in the
// call (not in its conversation, not its meeting's host or invitee). The same
// words whether or not a call is going on there.
var ErrNoCompanionHere = &helpers.CallRefusal{Code: "not_in_call", Msg: "You're not in this call."}

// ErrNobodyToAccompany: a companion asked for while nobody is in the call: a
// companion goes with a call that is going on, and doesn't start one.
var ErrNobodyToAccompany = &helpers.CallRefusal{Code: "call_not_started", Msg: "Nobody is in this call yet. Join it instead.", Status: 409}

// CompanionToken mints the token of a new companion of user in the call in a
// room: a member who may be in it now (MemberMayBeInCall), not put out of it
// and not new to it while it is locked (MayEnter), while someone is in its
// own room. The token is the one minter's (GenerateToken), on an identity the server
// draws, by the person's name in the call, and lets the companion publish only
// a screen, as the call's holds let them (SourcesAtDoor): never a microphone
// or a camera, and it never runs the room.
func CompanionToken(ctx context.Context, room string, user *dgraphStruct.DgraphUser, language string) (string, error) {
	if user == nil || !helpers.IsDgraphUID(user.Uid) {
		return "", ErrNoCompanionHere
	}
	ok, err := MemberMayBeInCall(ctx, room, user.Uid)
	if err != nil {
		return "", unchecked(ctx, room, err)
	}
	if !ok {
		return "", ErrNoCompanionHere
	}
	if err := MayEnter(ctx, room, user, false); err != nil {
		return "", err
	}
	// Someone in the call's own room, where a companion goes (not only in its
	// breakout rooms: there would be nobody to see).
	if active, err := livekitInit.LiveKitService.RoomHasHumanParticipants(ctx, room); err != nil {
		return "", unchecked(ctx, room, err)
	} else if !active {
		return "", ErrNobodyToAccompany
	}
	sources, err := SourcesAtDoor(ctx, room, user, false)
	if err != nil {
		return "", err
	}
	identity, err := livekitInit.NewCompanionIdentity(user.Uid)
	if err != nil {
		return "", err
	}
	return GenerateToken(ctx, room, identity, callName(room, user), false, language, sources)
}
