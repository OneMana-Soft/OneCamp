package business

// A member saying no to a DM's or a group's call ringing them (slack-06 H2,
// POST /calls/{session_id}/decline), or, from in the call, ringing its people
// again (POST /calls/{session_id}/ring). The ring, and what a decline tells
// whom, are business/Chat's (callRing.go); this holds who may do either, and
// records a decline once. The ended call's card names them (business/Huddle).

import (
	"context"
	"errors"
	"strings"
	"time"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

var (
	// ErrNothingToDecline is a call someone can't decline: none by that id,
	// one that is over, one that isn't a DM's or a group's, or one in a
	// conversation they aren't in. The calls' routes answer it 404, as they
	// answer a call someone may not see: nothing says it is there.
	ErrNothingToDecline = errors.New("callstate: no call here to decline")
	// ErrDeclineInCall is someone declining a call they are in.
	ErrDeclineInCall = errors.New("callstate: declining a call they are in")
	// ErrRingNotInCall is someone ringing a call's people again from outside it.
	ErrRingNotInCall = errors.New("callstate: ringing a call again from outside it")
)

// Decline records that who said no to the call sessionID ringing them, and
// answers the call and whether the decline is new: a repeat changes nothing.
// Only a member of the DM or group whose call is going on may decline it
// (MayRead, never someone in by a call's invite), and never while in it.
func Decline(ctx context.Context, who *userModels.UserInfo, sessionID string) (*Session, bool, error) {
	s, in, err := memberCall(ctx, who, sessionID)
	if err != nil {
		return nil, false, err
	}
	if in {
		return s, false, ErrDeclineInCall
	}
	me := who.UserPostgresInfo.Id
	id, err := uuid.Parse(s.ID)
	if err != nil {
		return nil, false, err
	}
	first, err := model.Decline(ctx, id, me, time.Now())
	if err != nil {
		return nil, false, err
	}
	return s, first, nil
}

// MayRingAgain answers the call sessionID if who may ring its people again:
// a member of its DM or group, as for a decline (ErrNothingToDecline
// otherwise), who is in it (ErrRingNotInCall otherwise). How often, and to
// whom, are business/Chat's RingAgain.
func MayRingAgain(ctx context.Context, who *userModels.UserInfo, sessionID string) (*Session, error) {
	s, in, err := memberCall(ctx, who, sessionID)
	if err != nil {
		return nil, err
	}
	if !in {
		return nil, ErrRingNotInCall
	}
	return s, nil
}

// memberCall is the DM's or group's call sessionID going on, when who is a
// member of its conversation (MayRead, never someone in by a call's invite),
// and whether who is in it; ErrNothingToDecline for any other call.
func memberCall(ctx context.Context, who *userModels.UserInfo, sessionID string) (*Session, bool, error) {
	s, err := Get(ctx, sessionID)
	if err != nil {
		return nil, false, err
	}
	me := who.UserPostgresInfo.Id
	if s == nil || !s.Live || me == uuid.Nil ||
		(s.Resource.Kind != conversationBusiness.KindDM && s.Resource.Kind != conversationBusiness.KindGroup) {
		return nil, false, ErrNothingToDecline
	}
	member, invitee, err := MayRead(ctx, who, s.Resource)
	if err != nil {
		return nil, false, err
	}
	if !member || invitee {
		return nil, false, ErrNothingToDecline
	}
	for _, p := range s.InCall() {
		if strings.EqualFold(p.UserUUID, me.String()) {
			return s, true, nil
		}
	}
	return s, false, nil
}
