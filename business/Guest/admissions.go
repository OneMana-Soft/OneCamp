package business

// Members let into a call through a link they opened as themselves (calls
// HU13): at a meeting's link or a room's address for as long as the link
// lives, or through one call's invite for that call alone. Every call check
// asks through business/LiveKit (RoomsAdmittedTo), which this package answers.

import (
	"context"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// liveAdmissionsOf is the model's read, a seam for tests.
var liveAdmissionsOf = callStateModel.LiveAdmissionsOf

func init() {
	liveKitBusiness.AnswerAdmissionsWith(roomsAdmittedTo)
}

// roomsAdmittedTo answers the call rooms a member may be in now through links
// they opened as themselves, in one read: none while guest access is off, and
// a room's address only while rooms are on.
func roomsAdmittedTo(ctx context.Context, user uuid.UUID) (map[string]bool, error) {
	rooms := map[string]bool{}
	if user == uuid.Nil || !guestOn() {
		return rooms, nil
	}
	links, err := liveAdmissionsOf(ctx, user, linkCapDays(ctx))
	if err != nil {
		return map[string]bool{}, err
	}
	addressesOn := roomsOn()
	for _, l := range links {
		if !l.Standing || addressesOn {
			rooms[l.Room] = true
		}
	}
	return rooms, nil
}
