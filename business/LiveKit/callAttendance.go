package business

// Who was in one call.
//
// Kept per call (its session), never per room: a channel's room is the same
// for every call in it, and attendance kept by room for 12 hours gathered
// everyone from every call in the channel that day, so a later call's notes
// were shared with people from earlier ones. Call State (business/CallState)
// records it from LiveKit's webhook.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
)

// CallAttendees answers the members who joined a call (by their graph uid),
// named by its session key (helpers.CallSessionKeyFor). Guests and the agent
// are not members: attendance decides who gets the call's notes, and only
// members can open a document.
func CallAttendees(ctx context.Context, sessionKey string) ([]string, error) {
	sid := helpers.CallSessionID(sessionKey)
	if sid == "" {
		return nil, nil
	}
	s, err := callStateModel.SessionBySID(ctx, sid)
	if err != nil || s == nil {
		return nil, err
	}
	people, err := callStateModel.Attendees(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range people {
		if a.UserUUID != nil && helpers.IsDgraphUID(a.Identity) {
			out = append(out, a.Identity)
		}
	}
	return out, nil
}

// FinishedCall is a call that has just ended: its room, the session key its
// transcript and attendance are kept under, and when it ran.
type FinishedCall struct {
	Room       string
	SessionKey string
	Began      time.Time
	Ended      time.Time
}
