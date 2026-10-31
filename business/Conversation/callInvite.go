package business

// A call's invitees (calls HU13, agreed with CHAT-LEAD 21 Oct): someone let
// into a conversation's call through its invite may join that call, and
// reply in its thread while the call goes on. Nothing else in the
// conversation opens to them: not the conversation, its name, another
// thread, or a later call. Asked after every other rule, only for what no
// rule opened.
//
// The thread opens to them only through the call's thread surface (calls
// CALLS-3b, agreed with CHAT-LEAD-2, 22 Oct), which answers the call's span
// and nothing of the conversation. Its handlers mark the context
// (ForCallThread); every other thread route asks unmarked and finds nothing,
// exactly as for a thread that isn't there.

import (
	"context"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// ReasonCallInvite is a decision that came of a link someone opened as
// themselves (a call's invite, a meeting's link, a room's address): what it
// opens, it opens without the conversation (a call's reader is not told it).
const ReasonCallInvite = "call_invite"

// The two reads, seams for tests: which posts are a live call's post (by its
// room), and which call rooms someone was let into through a link.
var (
	liveCallsOfPosts = callStateModel.LiveCallsOfPosts
	roomsAdmittedTo  = liveKitBusiness.RoomsAdmittedTo
)

type callThreadKey struct{}

// ForCallThread marks ctx as the call's thread surface asking. Only its
// handlers set it, server-side, never from anything a client sends.
func ForCallThread(ctx context.Context) context.Context {
	return context.WithValue(ctx, callThreadKey{}, true)
}

// AskedForCallThread is whether ctx is the call's thread surface asking.
func AskedForCallThread(ctx context.Context) bool {
	on, _ := ctx.Value(callThreadKey{}).(bool)
	return on
}

// invitedPerson is the member a call's invite could have let in: a person
// acting themselves, never an agent.
func invitedPerson(who accessBusiness.Subject) (uuid.UUID, bool) {
	if who.AgentUUID != "" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(who.UserUUID)
	return id, err == nil && id != uuid.Nil
}

// undecided is the ids no rule opened.
func undecided(out map[string]accessBusiness.Decision, ids []string) []string {
	var left []string
	for _, id := range ids {
		if out[id].Level == accessBusiness.LevelNone {
			left = append(left, id)
		}
	}
	return left
}

// callInvites opens a live call's thread at reply to someone let into that
// call through its invite. One read for the posts' calls, one for the invites.
func callInvites(ctx context.Context, who accessBusiness.Subject, out map[string]accessBusiness.Decision, ids []string) error {
	if !AskedForCallThread(ctx) {
		return nil
	}
	person, ok := invitedPerson(who)
	left := undecided(out, ids)
	if !ok || len(left) == 0 {
		return nil
	}
	posts := make([]uuid.UUID, 0, len(left))
	for _, id := range left {
		if p, err := uuid.Parse(id); err == nil {
			posts = append(posts, p)
		}
	}
	calls, err := liveCallsOfPosts(ctx, posts)
	if err != nil || len(calls) == 0 {
		return err
	}
	rooms, err := roomsAdmittedTo(ctx, person)
	if err != nil {
		return err
	}
	for post, room := range calls {
		if rooms[room] {
			out[post.String()] = invited(LevelReply)
		}
	}
	return nil
}

// callRoomInvites lets someone let into a call through its invite join it.
func callRoomInvites(ctx context.Context, who accessBusiness.Subject, out map[string]accessBusiness.Decision, rooms []string) error {
	person, ok := invitedPerson(who)
	left := undecided(out, rooms)
	if !ok || len(left) == 0 {
		return nil
	}
	admitted, err := roomsAdmittedTo(ctx, person)
	if err != nil {
		return err
	}
	for _, room := range left {
		if admitted[room] {
			out[room] = invited(LevelPost)
		}
	}
	return nil
}

// invited is a decision that came of a call's invite. Pure.
func invited(level accessBusiness.Level) accessBusiness.Decision {
	return accessBusiness.Decision{Level: level, Was: level, Reasons: []string{ReasonCallInvite}}
}
