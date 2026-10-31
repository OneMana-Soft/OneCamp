package business

// People coming and going (LiveKit's webhook): recording them in Call State,
// telling the room their part, checking they may be in the call at all, and
// handing a call on when the people running it have all gone.

import (
	"context"
	"errors"
	"strings"
	"time"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
)

// roomLocks keeps one room's comings and goings in the order this server
// handles them: a join and a leave for the same call never cross.
var roomLocks helpers.KeyedLock

// eventOf is a LiveKit participant as Call State records them: a member with
// their uuid, anyone else (a guest, someone no longer a member) without.
func eventOf(ctx context.Context, room, roomSID string, p *livekit.ParticipantInfo, at time.Time) callStateBusiness.JoinEvent {
	e := callStateBusiness.JoinEvent{Room: room, RoomSID: roomSID, Identity: p.Identity, Name: p.Name, At: at}
	if u, err := liveKitBusiness.ActiveMember(ctx, p.Identity); err == nil && u != nil {
		if id, err := uuid.Parse(u.Uuid); err == nil {
			e.UserUUID = &id
		}
	}
	return e
}

// inConversation is whether a member can open the conversation a call's room
// belongs to, a seam for tests.
var inConversation = liveKitBusiness.MayOpenConversation

// roleRule is the part someone takes in a call they are joining (Call State's
// RoleRule): see the package's doc.
func roleRule(ctx context.Context, ref callStateBusiness.Ref, e callStateBusiness.JoinEvent, starter bool) string {
	if e.UserUUID == nil {
		return model.RoleGuest
	}
	// In a breakout room your part is your part in its call: the room's name
	// isn't a conversation's, and its door is the server's room token alone.
	if parentRoom, _, ok := callStateBusiness.ParentOfBreakout(e.Room); ok {
		return partInCall(ctx, parentRoom, e.Identity)
	}
	// A member let in through a call's invite isn't in its conversation (HU13):
	// they run nothing. A check that can't be answered gives that same part.
	if ref.Kind != callStateBusiness.KindMeeting {
		if in, err := inConversation(ctx, e.Room, e.Identity); err != nil || !in {
			return model.RoleInvitee
		}
	}
	if equalCall(ref.Kind) {
		return model.RoleParticipant
	}
	if starter && ref.Kind == conversationBusiness.KindChannel {
		return model.RoleHost
	}
	u, err := liveKitBusiness.ActiveMember(ctx, e.Identity)
	if err != nil || u == nil {
		return model.RoleParticipant
	}
	host, err := liveKitBusiness.HostByRule(ctx, e.Room, u, liveKitBusiness.IsWorkspaceAdmin(ctx, u.Uuid))
	if err == nil && host {
		return model.RoleHost
	}
	return model.RoleParticipant
}

// partInCall is someone's part in the live call in room, or an invitee's
// (who runs nothing) when the call or their place in it can't be read.
func partInCall(ctx context.Context, room, identity string) string {
	s, err := model.OpenSessionOf(ctx, room)
	if err != nil || s == nil {
		return model.RoleInvitee
	}
	a, err := model.AttendeeOf(ctx, s.ID, identity)
	if err != nil || a == nil {
		return model.RoleInvitee
	}
	return a.Role
}

// OnJoined records that someone connected to a call (participant_joined),
// tells the room their part in it, and checks, in the background, that they
// may be in it now.
func OnJoined(ctx context.Context, room, roomSID string, p *livekit.ParticipantInfo, at time.Time) {
	if !livekitInit.IsPerson(p) {
		return
	}
	roomLocks.Acquire(room, 0)
	s, person, err := callStateBusiness.Joined(ctx, eventOf(ctx, room, roomSID, p, at), roleRule)
	roomLocks.Release(room)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not record %s joining %s: %+v", p.Identity, room, err)
	} else if person != nil {
		seat(s, p, person)
	}
	admit(room, p.Identity)
}

// seat tells the room, in the background, everything about someone as they
// (re)join (tellPerson), and gives them what they may publish under the
// call's holds: a token minted before the holds last changed grants what was
// held then, and someone who runs the call by who they are takes their part
// only now.
func seat(s *callStateBusiness.Session, p *livekit.ParticipantInfo, person *callStateBusiness.Person) {
	tellPerson(s.Room, s.Resource.Kind, person)
	if sources := liveKitBusiness.PublishSources(s.Held, person.Allowed, model.RunsCall(person.Role)); !livekitInit.PublishesOnly(p, sources) {
		tellSources(s.Room, p.Identity, sources)
	}
}

// connectedPeople is who is connected to a room now (LiveKit's listing, the
// DISCONNECTED left out), and leaveOf records a leave in Call State: seams for
// tests.
var (
	connectedPeople = func(ctx context.Context, room string) ([]*livekit.ParticipantInfo, error) {
		return livekitInit.LiveKitService.ConnectedParticipants(ctx, room)
	}
	leaveOf = callStateBusiness.Left
)

// stillHere answers, from the connections in a room, whether anyone is still
// in its call once the connection gone has left (gone nil: anyone at all), and
// whether gone's own person is among them on another connection: they came
// back, or are in on a second device, and the old connection's leave came
// late. A connection is told apart by its SID, never by who it is, and only a
// person counts (livekitInit.IsPerson: not the agent, a recorder or a hidden
// watcher, the rule that also lights the call's dot). Every
// "is anyone still here" that ends a call asks this: the leave webhook
// (OnLeft) and the sweep. Pure.
func stillHere(people []*livekit.ParticipantInfo, gone *livekit.ParticipantInfo) (anyone, them bool) {
	for _, p := range people {
		if !livekitInit.IsPerson(p) || (gone != nil && p.Sid == gone.Sid) {
			continue
		}
		anyone = true
		if gone != nil && p.Identity == gone.Identity {
			them = true
		}
	}
	return anyone, them
}

// OnLeft records that someone left a call (participant_left), hands the call
// on if they were the last of the people running it, and answers whether
// nobody is left in it. A listing that fails records the leave and ends
// nothing: room_finished and the sweep remain the backstop.
func OnLeft(ctx context.Context, room, roomSID string, p *livekit.ParticipantInfo, at time.Time) (empty bool) {
	if !livekitInit.IsPerson(p) {
		return false
	}
	people, err := connectedPeople(ctx, room)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not list the people in %s after %s left: %+v", room, p.Identity, err)
		recordLeft(ctx, room, roomSID, p, at)
		return false
	}
	anyone, them := stillHere(people, p)
	if them {
		helpers.LogInfoWithContext(ctx, "business/CallControl an earlier connection of %s left %s; they are still in it", p.Identity, room)
		return false
	}
	recordLeft(ctx, room, roomSID, p, at)
	return !anyone
}

// recordLeft writes a leave to Call State, and hands the call on if they were
// the last of the people running it.
func recordLeft(ctx context.Context, room, roomSID string, p *livekit.ParticipantInfo, at time.Time) {
	roomLocks.Acquire(room, 0)
	defer roomLocks.Release(room)
	s, changed, err := leaveOf(ctx, eventOf(ctx, room, roomSID, p, at))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not record %s leaving %s: %+v", p.Identity, room, err)
		return
	}
	if changed && s != nil {
		handOn(ctx, s)
		if err := settleStage(ctx, s); err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not take %s's board off the stage in %s: %+v", p.Identity, room, err)
		}
	}
}

// OnEnded ends the call in a room: its last person left (last_left), or
// LiveKit closed the room (room_finished). It answers whether the call is
// over: false for one that goes on in its breakout rooms, whose own room's
// end is no end of the call, and after which nothing a call's end does may
// run.
func OnEnded(ctx context.Context, room, roomSID, reason string) bool {
	roomLocks.Acquire(room, 0)
	defer roomLocks.Release(room)
	s, _, err := callStateBusiness.End(ctx, room, roomSID, reason)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not end the call in %s: %+v", room, err)
	}
	return s == nil || !s.Live
}

// tellPerson sets, in the background, everything the room sees of someone as
// they (re)join: their part and what they may do, and their hand and feedback,
// which outlast a reconnect.
func tellPerson(room, kind string, p *callStateBusiness.Person) {
	attrs := personAttributes(kind, p)
	helpers.GoSafeNamed("calls.person", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := livekitInit.LiveKitService.SetAttributes(ctx, room, p.Identity, attrs); err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not tell %s their part in %s: %+v", p.Identity, room, err)
		}
	})
}

// personAttributes is what the room is told of someone: their part, what they
// may do, their hand, their feedback and what they were let through ("" takes
// an attribute away). Pure.
func personAttributes(kind string, p *callStateBusiness.Person) map[string]string {
	attrs := attributesOf(kind, p.Role)
	attrs[AttrHand] = handValue(p.HandRaisedAt)
	attrs[AttrFeedback] = p.Feedback
	attrs[AttrAllowed] = strings.Join(p.Allowed, ",")
	return attrs
}

// tellAttrs changes some of what the room sees of someone, in the background.
func tellAttrs(room, identity string, attrs map[string]string) {
	helpers.GoSafeNamed("calls.attrs", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := livekitInit.LiveKitService.SetAttributes(ctx, room, identity, attrs); err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not update %s in %s: %+v", identity, room, err)
		}
	})
}

// tellSources sets, in the background, what someone in the call may publish
// (business/LiveKit's PublishSources), as their part or a hold changed it.
func tellSources(room, identity string, sources []string) {
	helpers.GoSafeNamed("calls.sources", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		p, err := livekitInit.LiveKitService.Participant(ctx, room, identity)
		if err == nil && p != nil {
			_, err = giveSources(ctx, room, p, sources)
		}
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not set what %s may publish in %s: %+v", identity, room, err)
		}
	})
}

// tellPart sets, in the background, what the room sees of someone: their part
// in the call and what they may do.
func tellPart(room, kind, identity, role string) {
	helpers.GoSafeNamed("calls.part", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := livekitInit.LiveKitService.SetAttributes(ctx, room, identity, attributesOf(kind, role)); err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not tell %s their part in %s: %+v", identity, room, err)
		}
	})
}

// admit checks, in the background, that someone who just connected may be in
// the call, and puts them out if not: a token outlives the right it was
// minted for (removed from the channel, a link revoked, put out by a host, a
// call locked since).
func admit(room, identity string) {
	helpers.GoSafeNamed("calls.admit", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		ok, reason := mayStay(ctx, room, identity)
		if ok {
			return
		}
		if err := liveKitBusiness.RemoveFromCall(ctx, room, identity, liveKitBusiness.Remover{}, reason); err != nil && !errors.Is(err, liveKitBusiness.ErrNotInCall) {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not put %s out of %s: %+v", identity, room, err)
		}
	})
}

// mayStay answers whether someone connected to a call may stay in it, and if
// not, why. Only identities this server mints are judged: LiveKit's own
// participants (a recorder) are none of its business. A check that can't be
// answered is asked again a little later; if it still can't be, they are put
// out as unconfirmed (fail closed, 21 Oct), never let stay: a host's removal,
// a lock, a revoked link or a channel left is not skipped because a store
// blinked, and they can join again once it answers.
func mayStay(ctx context.Context, room, identity string) (bool, string) {
	for attempt := 0; ; attempt++ {
		ok, reason, err := stayCheck(ctx, room, identity)
		if err == nil {
			return ok, reason
		}
		if attempt >= len(stayRetries) || !sleepCtx(ctx, stayRetries[attempt]) {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not check whether %s may stay in %s: %+v", identity, room, err)
			return false, liveKitBusiness.RemovedUnconfirmed
		}
	}
}

// stayRetries are the waits before a check that couldn't be answered is asked
// again; within admit's 15 s.
var stayRetries = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}

// sleepCtx waits d, reporting false when ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// stayCheck is one asking of mayStay: may stay, or why not; an error when a
// store didn't answer.
func stayCheck(ctx context.Context, room, identity string) (bool, string, error) {
	// Whoever may stay in a call may stay in the breakout rooms of it they
	// may be in, and nobody else.
	if parentRoom, n, ok := callStateBusiness.ParentOfBreakout(room); ok {
		if ok, reason, err := roomStayCheck(ctx, parentRoom, n, identity); err != nil || !ok {
			return ok, reason, err
		}
		room = parentRoom
	}
	switch {
	case strings.HasPrefix(identity, "guest-"):
		ok, err := guestBusiness.GuestMayJoin(ctx, room, identity)
		if err != nil {
			return false, "", err
		}
		if !ok {
			return false, liveKitBusiness.RemovedLinkRevoked, nil
		}
	case liveKitBusiness.IsMemberIdentity(identity):
		ok, err := liveKitBusiness.MemberMayBeInCall(ctx, room, identity)
		if err != nil {
			return false, "", err
		}
		if !ok {
			return false, liveKitBusiness.RemovedNotAMember, nil
		}
	default:
		return true, "", nil
	}
	removed, err := liveKitBusiness.IsRemovedFromCall(ctx, room, identity)
	if err != nil {
		return false, "", err
	}
	if removed {
		return false, liveKitBusiness.RemovedByHost, nil
	}
	locked, err := liveKitBusiness.LockedOut(ctx, room, identity)
	if err != nil {
		return false, "", err
	}
	if locked {
		return false, liveKitBusiness.RemovedLocked, nil
	}
	return true, "", nil
}

// handOn makes the member who has been in a call longest its host once its
// last host and co-host have gone, so a call people are still in always has
// someone who can mute a noisy line. Not in a DM or a group, where everyone
// may already; never a guest.
func handOn(ctx context.Context, s *callStateBusiness.Session) {
	// A breakout room's parts are its call's; and while a call goes on in
	// its rooms, whoever runs it is in one of them, not gone.
	if !s.Live || equalCall(s.Resource.Kind) || s.Parent != "" || s.GoesOnInRooms(time.Now()) {
		return
	}
	in := s.InCall()
	for _, p := range in {
		if p.Role == model.RoleHost || p.Role == model.RoleCohost {
			return
		}
	}
	for _, p := range in {
		// Never a guest, nor someone in through a call's invite (HU13).
		if p.Guest || p.Removed || p.Role == model.RoleInvitee {
			continue
		}
		// The record can lag LiveKit by a webhook: only someone really there.
		if lp, err := livekitInit.LiveKitService.Participant(ctx, s.Room, p.Identity); err != nil || lp == nil {
			continue
		}
		changed, err := callStateBusiness.SetRole(ctx, s, p.Identity, model.RoleHost, resourceEventBusiness.Actor{System: true})
		if err != nil || !changed {
			return
		}
		tellPart(s.Room, s.Resource.Kind, p.Identity, model.RoleHost)
		tellSources(s.Room, p.Identity, livekitInit.CallPublishSources)
		if err := liveKitBusiness.TellInCall(ctx, s.Room, liveKitBusiness.CallPacket{T: "role", Role: model.RoleHost, Reason: "handed_on"}, p.Identity); err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not tell %s they host %s now: %+v", p.Identity, s.Room, err)
		}
		return
	}
}
