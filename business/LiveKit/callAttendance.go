package business

// Who was in one call.
//
// Kept per call (its session key), never per room: a channel's room is the
// same for every call in it, and attendance kept by room for 12 hours gathered
// everyone from every call in the channel that day, so a later call's notes
// were shared with people from earlier ones.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// NoteAttendee records that a member joined (or was in) a call. Guests and
// the agent are not members and are not recorded: attendance decides who
// gets the call's notes, and only members can open a document. Best-effort:
// a call must never fail because Redis did.
func NoteAttendee(ctx context.Context, sessionKey, identity string) {
	if !helpers.IsCallSessionKey(sessionKey) || !helpers.IsDgraphUID(identity) {
		return
	}
	if err := redisStore.AddToSet(ctx, registry.CallAttendees, []string{sessionKey}, identity); err != nil {
		helpers.LogErrorWithContext(ctx, "business/NoteAttendee could not record a call attendee: %+v", err)
	}
}

// CallAttendees answers the members who joined a call.
func CallAttendees(ctx context.Context, sessionKey string) ([]string, error) {
	if !helpers.IsCallSessionKey(sessionKey) {
		return nil, nil
	}
	return redisStore.GetSetMembers(ctx, registry.CallAttendees, []string{sessionKey})
}

// FinishedCall is a call that has just ended: its room, the session key its
// transcript and attendance are kept under, and when it ran.
type FinishedCall struct {
	Room       string
	SessionKey string
	Began      time.Time
	Ended      time.Time
}
