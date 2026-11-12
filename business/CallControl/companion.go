package business

import (
	"context"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// CompanionToken mints a member's token for another device of theirs in the
// call whose session is sessionID (meet-04): companion mode (chat, hands,
// reactions and presenting, without a microphone, a camera or a speaker) and
// presenting without joining, from the same token. Never through an API
// token, and only into a call's own room while it goes on (a breakout room's
// call has none), by business/LiveKit's rule for who may have one. Its
// identity is the server's (livekitInit's companion.go), never the client's.
func CompanionToken(ctx context.Context, actor *dgraphStruct.DgraphUser, sessionID, language string) (RoomToken, error) {
	if helpers.ActsThroughToken(ctx) {
		return RoomToken{}, ErrThroughToken
	}
	if actor == nil || actor.Uid == "" {
		return RoomToken{}, ErrNotInCall
	}
	s, err := liveCall(ctx, sessionID)
	if err != nil {
		return RoomToken{}, err
	}
	if s.Parent != "" {
		return RoomToken{}, ErrNotInCall
	}
	token, err := liveKitBusiness.CompanionToken(ctx, s.Room, actor, language)
	if err != nil {
		return RoomToken{}, err
	}
	return RoomToken{Token: token, Room: s.Room, Session: s.ID}, nil
}
