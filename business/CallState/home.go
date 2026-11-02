package business

// Where what a call makes lands (meet-02): its recording, its transcript, its
// notes and its recap. One answer for every one of them, so none of them
// guesses at the room's name again.

import (
	"context"

	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// meetingOf reads a meeting (a seam for tests).
var meetingOf = model.MeetingOf

// Home is where a call keeps what it makes. A conversation's call keeps it in
// the conversation; an event's call keeps it on the event; a breakout room
// keeps it where its call does.
type Home struct {
	// Ref is what the call belongs to: a channel, a DM, a group, or a meeting.
	Ref Ref
	// Event is the calendar event whose call the meeting is; uuid.Nil for a
	// conversation's call, and for any meeting that is no event's.
	Event uuid.UUID
}

// HomeOf answers where the call in a room keeps what it makes, reading the
// meeting for a meeting's room and nothing for a conversation's:
// ErrNotACallRoom for a name no call uses. A meeting with no row (an instant
// meeting's room nobody recorded) is a meeting with no event.
func HomeOf(ctx context.Context, room string) (Home, error) {
	ref, _, ok := RefOfRoom(room)
	if !ok {
		return Home{}, ErrNotACallRoom
	}
	if ref.Kind != KindMeeting {
		return Home{Ref: ref}, nil
	}
	m, err := meetingOf(ctx, ref.ID)
	if err != nil {
		return Home{}, err
	}
	home := Home{Ref: ref}
	if m != nil {
		home.Event = m.Event
	}
	return home, nil
}
