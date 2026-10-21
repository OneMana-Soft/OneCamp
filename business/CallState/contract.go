// Package business (CallState) is Call State (design/calls-vs-zoom/PROGRAM.md
// §4 #29): the record of every call, built once for calls and for chat's
// huddles (design/chat-vs-slack/PROGRAM.md B7), by the contract the two
// programmes agreed on 17 Oct 2026.
//
// A call used to be a LiveKit room and nothing more. Now each call is a
// session, from the first person who joins to the moment the last one leaves,
// with who was in it (members and guests), their part in it (host, co-host,
// participant, guest), what the people running it set (a lock, a spotlight,
// people joining muted) and the recordings made during it. Everything a call
// produces hangs off the session (CL1).
//
// WHO WRITES. Only this package writes the session tables, from LiveKit's
// webhook (Joined, Left, End) and the reconcile sweep (reconcile.go), and for
// Call Control's actions (changes.go). Chat's huddles write the post and the
// topic through functions added here when they land (the contract's
// AttachPost and SetTopic).
//
// WHAT OTHERS HEAR. Each change goes out three ways:
//   - Resource Events (business/ResourceEvent): call.started and call.ended
//     once per call, call.joined at every join (rejoin=true after the first),
//     call.left at every leave, and the host actions' kinds. ResourceKind is
//     the conversation's kind (or "meeting") and ResourceUUID its Ref's ID.
//   - MQTT call_state to the conversation's people (publish.go), at most once a
//     second per call, for the header's faces and the green room.
//   - LiveKit itself, for the people in the call (business/CallControl sets
//     their part as a participant attribute).
//
// IMPORTS. One way: this package imports business/Conversation (for Ref) and
// business/LiveKit; neither of them, nor business/Chat or business/Channel,
// may import this one. business/LiveKit reads the rows it needs at token time
// (a lock, a removal) from models/postgres/CallState directly.
package business

import (
	"errors"
	"time"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
)

// Ref names what a call belongs to: a conversation (business/Conversation's
// Ref, whose kinds are channel, dm and group) or a meeting.
type Ref = conversationBusiness.Ref

// KindMeeting is a call with a link of its own (an instant meeting), which
// belongs to no conversation. Its Ref's ID is the meeting's id.
const KindMeeting = "meeting"

var (
	// ErrNotACallRoom is a room name no call uses.
	ErrNotACallRoom = errors.New("callstate: not a call room")
	// ErrCallOver is a call that has ended: nothing can be changed about it.
	ErrCallOver = errors.New("callstate: the call is over")
)

// Person is someone in a call, or who was.
type Person struct {
	// Identity is who LiveKit says they are: a member's graph uid, or a guest's
	// "guest-…" identity.
	Identity string `json:"identity"`
	// UserUUID is a member's; empty for a guest.
	UserUUID string `json:"user_uuid,omitempty"`
	Name     string `json:"name"`
	// Role is their part in this call: host, cohost, participant or guest.
	Role  string `json:"role"`
	Guest bool   `json:"guest,omitempty"`
	// In is whether they are in the call now.
	In        bool       `json:"in_call"`
	JoinedAt  time.Time  `json:"joined_at"`
	LeftAt    *time.Time `json:"left_at,omitempty"`
	SecondsIn int        `json:"seconds_in"`
	// Removed is whether someone running the call put them out of it.
	Removed bool `json:"removed,omitempty"`
}

// Lock is who locked a call, so nobody new can join it, and when.
type Lock struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

// Recording is a recording made during a call.
type Recording struct {
	EgressID  string     `json:"egress_id"`
	StartedBy string     `json:"started_by,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Status    string     `json:"status"`
}

// Session is one call, as everyone who may see it reads it.
type Session struct {
	ID string `json:"id"`
	// Kind is huddle (a call in a conversation) or meeting.
	Kind     string `json:"kind"`
	Resource Ref    `json:"resource"`
	Room     string `json:"room"`
	// RoomSID is LiveKit's id for the room this call runs in.
	RoomSID     string     `json:"-"`
	Live        bool       `json:"live"`
	StartedAt   time.Time  `json:"started_at"`
	StartedBy   string     `json:"started_by,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	EndedReason string     `json:"ended_reason,omitempty"`
	// PostUUID and Topic are the huddle's post and name (chat's huddles).
	PostUUID string `json:"post_uuid,omitempty"`
	Topic    string `json:"topic,omitempty"`
	Locked   *Lock  `json:"locked,omitempty"`
	// Spotlight is the identity everyone's stage shows large, if anyone's.
	Spotlight   string `json:"spotlight,omitempty"`
	MuteOnEntry bool   `json:"mute_on_entry,omitempty"`
	// People is everyone who was in the call, in the order they first joined.
	People     []Person    `json:"people"`
	Recordings []Recording `json:"recordings"`
}

// InCall answers the people in the call now.
func (s *Session) InCall() []Person {
	var out []Person
	for _, p := range s.People {
		if p.In {
			out = append(out, p)
		}
	}
	return out
}

// Person answers one person's part in the call, or nil.
func (s *Session) Person(identity string) *Person {
	for i := range s.People {
		if s.People[i].Identity == identity {
			return &s.People[i]
		}
	}
	return nil
}
