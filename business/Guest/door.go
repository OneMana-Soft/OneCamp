package business

// Doors: a link someone opens to come into a call, and the call it opens. A
// meeting's link (or a room's address) opens the meeting's own room; a call's
// invite (calls HU13) opens its call's room while that call goes on, and
// nothing once it is over: a later call in the same conversation is another
// call, which the invite never opens.

import (
	"context"

	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// sessionByID is the call a call's invite opens, a seam for tests.
var sessionByID = callStateModel.SessionByID

// Door is a link and the call room it opens.
type Door struct {
	Grant *guestModel.GuestGrant
	Room  string
}

// doorRoom answers the call room a grant opens now: a meeting's own room, or
// a call's invite's call while it goes on. Anything else, or a call that is
// over, opens nothing (ErrInvalidGrant); a read that failed is that error.
func doorRoom(ctx context.Context, g *guestModel.GuestGrant) (string, error) {
	switch g.ResourceType {
	case guestModel.ResourceMeeting:
		return g.ResourceID, nil
	case guestModel.ResourceCall:
		id, err := uuid.Parse(g.ResourceID)
		if err != nil {
			return "", ErrInvalidGrant
		}
		s, err := sessionByID(ctx, id)
		if err != nil {
			return "", err
		}
		if s == nil || !s.Open() {
			return "", ErrInvalidGrant
		}
		return s.Room, nil
	}
	return "", ErrInvalidGrant
}

// OpenDoor resolves a link's raw token to the call it lets someone into: a
// meeting's link, or a call's invite while its call goes on. Anything that
// opens nothing is the one sentinel the routes answer alike (no oracle); a
// read that failed is that error.
func OpenDoor(ctx context.Context, rawToken string) (Door, error) {
	g, err := ResolveExternal(ctx, External{Token: rawToken})
	if err != nil {
		return Door{}, err
	}
	return DoorOf(ctx, g)
}

// DoorOf is the door of a grant already checked (a room's address and its
// unlock): the call room it opens now.
func DoorOf(ctx context.Context, g *guestModel.GuestGrant) (Door, error) {
	room, err := doorRoom(ctx, g)
	if err != nil {
		return Door{}, err
	}
	return Door{Grant: g, Room: room}, nil
}
