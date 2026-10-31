package business

// Members who came in through a link as themselves (calls HU13), when the link
// ends: revoked, rooms or guest access turned off, a room closed, a call over.
// And a call's invite, made here for business/CallInvite.

import (
	"context"
	"errors"
	"time"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/helpers"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// The reads and the putting out, seams for tests.
var (
	admittedThrough     = callStateModel.AdmittedThrough
	admittedInLiveCalls = callStateModel.AdmittedInLiveCalls
	admittedIn          = callStateModel.AdmittedIn
	memberMayBeIn       = liveKitBusiness.MemberMayBeInCall
	removeFromCall      = func(ctx context.Context, room, identity string) error {
		return liveKitBusiness.RemoveFromCall(ctx, room, identity, liveKitBusiness.Remover{}, liveKitBusiness.RemovedLinkRevoked)
	}
)

// putOutLinkMembers puts out of their calls the members who came in through
// links that have ended, unless something else still lets them in: their
// conversation, their own meeting, another live link. A check that can't be
// answered puts them out (fail closed): they can come back by whatever still
// lets them in. One check per person; they are few.
func putOutLinkMembers(ctx context.Context, admitted []callStateModel.Admission) {
	for _, a := range admitted {
		if ok, err := memberMayBeIn(ctx, a.Room, a.Identity); err == nil && ok {
			continue
		}
		if err := removeFromCall(ctx, a.Room, a.Identity); err != nil && !errors.Is(err, liveKitBusiness.ErrNotInCall) {
			helpers.LogErrorWithContext(ctx, "business/Guest could not put %s out of %s: %+v", a.Identity, a.Room, err)
		}
	}
}

// putOutMembersOf puts out the members who came in through these links.
func putOutMembersOf(ctx context.Context, grants []uuid.UUID) {
	admitted, err := admittedThrough(ctx, grants)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Guest could not list who came in through %d links: %+v", len(grants), err)
		return
	}
	putOutLinkMembers(ctx, admitted)
}

// CallLink is a call's invite as its maker gets it: the raw token, shown once
// (only its hash is kept), and when it stops working at the latest. The call's
// end comes first.
type CallLink struct {
	GuestToken string    `json:"guest_token"`
	GrantID    string    `json:"grant_id"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// NewCallLink makes a call's invite (HU13): a guest grant on the call (its
// session) that opens that call alone, and ends with it. The caller has checked
// that its maker may let people into the call (business/CallInvite).
func NewCallLink(ctx context.Context, session, by uuid.UUID) (*CallLink, error) {
	if !guestOn() {
		return nil, ErrGuestDisabled
	}
	raw, err := randomToken()
	if err != nil {
		return nil, err
	}
	expiresAt := time.Now().Add(meetingGrantTTL)
	grantID, err := guestModel.CreateGrant(ctx, hashToken(raw), guestModel.ResourceCall, session.String(), guestModel.CapabilityJoin, by, &expiresAt)
	if err != nil {
		return nil, err
	}
	return &CallLink{GuestToken: raw, GrantID: grantID.String(), ExpiresAt: expiresAt}, nil
}

// EndCallLinks ends a call's invites as the call ends, so the admin's list
// shows only live ones. Every check also asks that the call goes on, so this
// is tidiness, not the guard.
func EndCallLinks(ctx context.Context, session uuid.UUID) error {
	_, err := guestModel.RevokeLiveForResource(ctx, guestModel.ResourceCall, session.String())
	return err
}
