package business

// Who a call's participant is, proved by their own call token: how a guest,
// who has no session, acts on themselves in a call (their hand, their
// feedback). The token is the one this server minted for them to join, signed
// with the LiveKit secret, so it names exactly one identity in one room.

import (
	"context"
	"errors"
	"strings"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/livekit/protocol/auth"
)

// ErrBadCallToken is a call token this server didn't sign, or one for another room.
var ErrBadCallToken = errors.New("callcontrol: not a call token for this call")

// guestPrefix is how a guest's LiveKit identity starts.
const guestPrefix = "guest-"

// GuestOf answers the guest a call token belongs to, and the room it lets them
// into, when this server signed it and it is a guest's. A member's token is
// refused: members act through their own session.
func GuestOf(raw string) (identity, room string, err error) {
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
	if raw == "" || livekitInit.LiveKitService.Config == nil || livekitInit.LiveKitService.Config.ApiSecret == "" {
		return "", "", ErrBadCallToken
	}
	v, err := auth.ParseAPIToken(raw)
	if err != nil || v.APIKey() != livekitInit.LiveKitService.Config.ApiKey {
		return "", "", ErrBadCallToken
	}
	grants, err := v.Verify(livekitInit.LiveKitService.Config.ApiSecret)
	if err != nil || grants == nil || grants.Video == nil || grants.Video.Room == "" {
		return "", "", ErrBadCallToken
	}
	identity = grants.Identity
	if !strings.HasPrefix(identity, guestPrefix) {
		return "", "", ErrBadCallToken
	}
	return identity, grants.Video.Room, nil
}

// GuestActsOnThemselves does a guest's own action (their hand or feedback) in
// the call going on in the room their token is for: a guest's page knows its
// token, not the call's session.
func GuestActsOnThemselves(ctx context.Context, token string, a Action) (Result, error) {
	identity, room, err := GuestOf(token)
	if err != nil {
		return Result{}, ErrNotInCall
	}
	if !OwnAction(a, identity) {
		return Result{}, ErrNotAllowed
	}
	// Rooms are chosen in the call's own session, from any of its rooms.
	if parent, _, ok := callStateBusiness.ParentOfBreakout(room); ok && a.Action == ActBreakoutChoose {
		room = parent
	}
	s, err := guestSession(ctx, room)
	if err != nil {
		return Result{}, err
	}
	return ActOnYourself(ctx, identity, s.ID, a)
}

// guestSession is the call going on in the room a guest's call token is for:
// a call's own room, or one of its breakout rooms.
func guestSession(ctx context.Context, room string) (*callStateBusiness.Session, error) {
	if _, _, ok := callStateBusiness.RefOfRoom(room); !ok {
		return nil, ErrNotInCall
	}
	s, err := callStateBusiness.InRoom(ctx, room)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, ErrNotInCall
	}
	return s, nil
}
