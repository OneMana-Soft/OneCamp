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

// human reports whether a LiveKit participant is a person: not the
// transcription agent, not a hidden recorder. Pure.
func human(p *livekit.ParticipantInfo) bool {
	if p == nil || strings.TrimSpace(p.Identity) == "" || livekitInit.IsAgentIdentity(p.Identity) {
		return false
	}
	if p.Permission != nil && p.Permission.Hidden {
		return false
	}
	return p.Kind != livekit.ParticipantInfo_EGRESS && p.Kind != livekit.ParticipantInfo_AGENT
}

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

// roleRule is the part someone takes in a call they are joining (Call State's
// RoleRule): see the package's doc.
func roleRule(ctx context.Context, ref callStateBusiness.Ref, e callStateBusiness.JoinEvent, starter bool) string {
	if e.UserUUID == nil {
		return model.RoleGuest
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

// OnJoined records that someone connected to a call (participant_joined),
// tells the room their part in it, and checks, in the background, that they
// may be in it now.
func OnJoined(ctx context.Context, room, roomSID string, p *livekit.ParticipantInfo, at time.Time) {
	if !human(p) {
		return
	}
	roomLocks.Acquire(room, 0)
	s, person, err := callStateBusiness.Joined(ctx, eventOf(ctx, room, roomSID, p, at), roleRule)
	roomLocks.Release(room)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not record %s joining %s: %+v", p.Identity, room, err)
	} else if person != nil {
		tellPart(s.Room, s.Resource.Kind, person.Identity, person.Role)
	}
	admit(room, p.Identity)
}

// OnLeft records that someone left a call (participant_left), and hands the
// call on if they were the last of the people running it.
func OnLeft(ctx context.Context, room, roomSID string, p *livekit.ParticipantInfo, at time.Time) {
	if !human(p) {
		return
	}
	roomLocks.Acquire(room, 0)
	defer roomLocks.Release(room)
	s, changed, err := callStateBusiness.Left(ctx, eventOf(ctx, room, roomSID, p, at))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not record %s leaving %s: %+v", p.Identity, room, err)
		return
	}
	if changed && s != nil {
		handOn(ctx, s)
	}
}

// OnEnded ends the call in a room: its last person left (last_left), or
// LiveKit closed the room (room_finished).
func OnEnded(ctx context.Context, room, roomSID, reason string) {
	roomLocks.Acquire(room, 0)
	defer roomLocks.Release(room)
	if _, _, err := callStateBusiness.End(ctx, room, roomSID, reason); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not end the call in %s: %+v", room, err)
	}
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
	if !s.Live || equalCall(s.Resource.Kind) {
		return
	}
	in := s.InCall()
	for _, p := range in {
		if p.Role == model.RoleHost || p.Role == model.RoleCohost {
			return
		}
	}
	for _, p := range in {
		if p.Guest || p.Removed {
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
		if err := liveKitBusiness.TellInCall(ctx, s.Room, liveKitBusiness.CallPacket{T: "role", Role: model.RoleHost, Reason: "handed_on"}, p.Identity); err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallControl could not tell %s they host %s now: %+v", p.Identity, s.Room, err)
		}
		return
	}
}
