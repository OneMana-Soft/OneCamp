package business

// What an invitation is, as the people on either end of it see it.
//
// An invitation used to say "Sent" until somebody opened its expired link, so
// an admin could not tell a week-old dead invitation from a live one; it said
// "Sent" forever when the person joined through Google or single sign-on,
// because only the password sign-up marked it; an expired one still let its
// owner in through Google; and inviting someone who was already a member sent
// them an invitation to a workspace they were in. The state is now derived
// when it is read, joining marks it whatever the way in (JoinAsMember), only a
// live one admits anyone, and inviting a member is refused in words the admin
// can act on.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
)

// What an invitation can be, as stored (userModels.InvitationStatus*), and
// whether it is live: userModels.Invitation.LiveAt, the one predicate.
const (
	InvitationPending = userModels.InvitationStatusPending
	InvitationSent    = userModels.InvitationStatusSent
	InvitationExpired = userModels.InvitationStatusExpired
	InvitationJoined  = userModels.InvitationStatusJoined
)

// InvitationTTL is how long an invitation's link works.
const InvitationTTL = userModels.InvitationLinkLifetime

// ErrInvitationExpired turns away, through Google or GitHub, an address
// whose invitation was never used and is past its expiry, for the sign-in
// page to say so. (No invitation at all is ErrNotInvited.)
var ErrInvitationExpired = errors.New("your invitation has expired; ask whoever invited you to send it again")

// InviteRefusal is why an invitation was not made or sent again, in words for
// the admin who asked.
type InviteRefusal struct{ Msg string }

func (e *InviteRefusal) Error() string { return e.Msg }

// IsInviteRefusal reports whether err is an InviteRefusal, and returns it.
func IsInviteRefusal(err error) (*InviteRefusal, bool) {
	var r *InviteRefusal
	ok := errors.As(err, &r)
	return r, ok
}

// InvitationLink is the address an invitation's link opens: the sign-up page
// on this workspace's web app, which carries the scheme (FrontendBaseURL).
func InvitationLink(token string) string {
	return invitationLinkOn(authService.FrontendBaseURL(), token)
}

// invitationLinkOn is InvitationLink on a given base. Pure.
//
// WHY THE BASE MATTERS. The email used to build its link from FE_HOST_DOMAIN
// as written, and as written it has no scheme: make install sets it to
// onecamp.<domain>. An <a href="onecamp.example.com/signup?token=..."> is a
// RELATIVE link to a mail client, so every invitation this server sent
// pointed nowhere. FrontendBaseURL adds the scheme and honours FRONTEND_DOMAIN.
func invitationLinkOn(base, token string) string {
	return fmt.Sprintf("%s/signup?token=%s", strings.TrimRight(base, "/"), token)
}

// InvitationView is an invitation as the admin's list shows it.
type InvitationView struct {
	Id        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	InvitedBy uuid.UUID `json:"invited_by"`
	// Status is "sent" (or "pending", from before tokens) while it can still
	// be used, "expired" once its link has run out, and "joined" once used.
	Status    string     `json:"status"`
	ExpiresAt *time.Time `json:"token_expires_at,omitempty"`
	// ExpiresInDays is how many days its link has left, rounded up, while it
	// can still be used.
	ExpiresInDays *int `json:"expires_in_days,omitempty"`
	// InviteLink is the link it was sent with, for the admin to copy, while it
	// can still be used.
	InviteLink string    `json:"invite_link,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// InvitationViewAt is an invitation as it stands at now: expired once its
// link has run out, whatever the row still says. link builds the link from
// its token. Pure.
func InvitationViewAt(inv *userModels.Invitation, now time.Time, link func(token string) string) InvitationView {
	expires := inv.ExpiresAt()
	view := InvitationView{
		Id:        inv.Id,
		Email:     inv.Email,
		InvitedBy: inv.InvitedBy,
		Status:    inv.Status,
		ExpiresAt: &expires,
		CreatedAt: inv.CreatedAt,
	}
	if view.Status == "" {
		view.Status = InvitationPending
	}
	if view.Status == InvitationJoined || view.Status == InvitationExpired {
		return view
	}
	if !inv.LiveAt(now) {
		view.Status = InvitationExpired
		return view
	}
	days := int(math.Ceil(expires.Sub(now).Hours() / 24))
	view.ExpiresInDays = &days
	if inv.Token != nil && *inv.Token != "" {
		view.InviteLink = link(*inv.Token)
	}
	return view
}

// InvitationLinkProblem is why someone holding an invitation's link cannot
// use it, in words for them, or "" when they can. inviter is who made it, to
// name the person to ask for another ("" for whoever invited them). Pure.
func InvitationLinkProblem(inv *userModels.Invitation, now time.Time, inviter string) string {
	if inv == nil {
		return "This invitation link isn't valid. Ask whoever invited you to send it again."
	}
	if inv.Status == InvitationJoined {
		return "This invitation has been used already. Sign in instead."
	}
	if !inv.LiveAt(now) {
		if strings.TrimSpace(inviter) == "" {
			inviter = "whoever invited you"
		}
		return "This invitation has expired. Ask " + inviter + " to send it again."
	}
	return ""
}

// ListInvitations is every invitation, newest first, as it stands now.
func ListInvitations(ctx context.Context) ([]InvitationView, error) {
	invitations, err := GetAllInvitations(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]InvitationView, 0, len(invitations))
	for _, inv := range invitations {
		out = append(out, InvitationViewAt(inv, now, InvitationLink))
	}
	return out, nil
}

// refuseInvitingMember refuses an invitation to someone who already has an
// account here, saying what to do instead.
func refuseInvitingMember(ctx context.Context, email string) error {
	exists, deactivated, err := domain.MemberAccountState(ctx, email)
	if err != nil {
		return err
	}
	switch {
	case exists && deactivated:
		return &InviteRefusal{Msg: fmt.Sprintf("%s already has an account here, which is deactivated. Reactivate it under Users instead of inviting them.", email)}
	case exists:
		return &InviteRefusal{Msg: fmt.Sprintf("%s is already a member of this workspace.", email)}
	}
	return nil
}

// memberInviteRefusal is what a member is told whenever an address can't be
// invited: one answer for an account (live or deactivated), an invitation
// (live or expired) and anything else, so it says nothing about the address.
var memberInviteRefusal = &InviteRefusal{Msg: "That address can't be invited from here. If they should join, ask an admin."}

// RenewInvitation gives an existing invitation a new link and expiry, as
// sending it again does (an admin's action), and makes it renewedBy's.
// Refused for a member, for an invitation already used, and for an address
// with no invitation.
func RenewInvitation(ctx context.Context, email string, renewedBy uuid.UUID, token string, expiresAt time.Time) error {
	email = helpers.NormalizeEmail(email)
	if err := refuseInvitingMember(ctx, email); err != nil {
		return err
	}
	existing, err := domain.GetInvitationByEmail(ctx, email)
	if err != nil {
		return errors.New("failed to read the invitation")
	}
	if existing == nil {
		return &InviteRefusal{Msg: fmt.Sprintf("There is no invitation for %s to send again. Invite them instead.", email)}
	}
	if existing.Status == InvitationJoined {
		return &InviteRefusal{Msg: fmt.Sprintf("%s has already joined with this invitation.", email)}
	}
	if err := domain.UpdateInvitationTokenByID(ctx, existing.Id, token, expiresAt, renewedBy); err != nil {
		helpers.LogErrorWithContext(ctx, "business/RenewInvitation Failed to update invitation token err: %+v", err)
		return errors.New("failed to update invitation token")
	}
	return nil
}
