package business

// A board's link with a role, a password and an end (miro-33 HEAD A).
//
// A board link lets its guest view, comment or edit (draw). Its guest is the
// link, a named guest grant: on the board live as guest-<grant id>, named as
// they typed it under the "Guest: " marker inside the collab token the server
// signs, never a user row (no member list, mention picker or seat count reads
// grants). What a guest writes is authored from that token, never from what
// the client sends.
//
// A password is the docs' publish password (C6): the same rule for a new one
// (SecretProblem), the same hash (hashSecret), the same tries' limits per
// address and per link (UnlockTryAllowed) and the same unlocks (MintUnlock,
// Unlocks), carried as ?k= or in the exchange's body. A new password, or none
// where there was one, moves the link's unlock version: every unlock and
// live token given before stops.
//
// Only who may manage the board (its owner and co-owners) makes, changes or
// turns off its links (controllers/Guest mayShare); every change is audited
// there. No plan gates any of it.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// The refusals a password link's guest gets.
var (
	// ErrPasswordNeeded is a password link opened without one: 401.
	ErrPasswordNeeded = errors.New("guest: this link needs its password")
	// ErrWrongPassword is a password that doesn't open the link: 401.
	ErrWrongPassword = errors.New("guest: that password isn't right")
	// ErrTooManyTries is one try past the limits (per address and link, and
	// per link): 429.
	ErrTooManyTries = errors.New("guest: too many password tries")
	// ErrNoRole is a role a board link doesn't take: 400.
	ErrNoRole = &PublishInvalid{"A board link lets its guest view, comment or edit."}
	// ErrNotABoardLink is a change asked of a link that isn't a live board link.
	ErrNotABoardLink = errors.New("guest: not a live board link")
	// ErrLinkChangedMeanwhile is a change made from a read of the link that
	// someone else's change has since made stale: nothing is written. 409.
	ErrLinkChangedMeanwhile = errors.New("guest: the link changed meanwhile")
)

// linkCapability is the role a link to resourceType gets when asked for
// asked: a board's view, comment or edit; a doc's or a project's view or
// comment; a channel's view or post; view for anything else.
func linkCapability(resourceType, asked string) string {
	switch {
	case resourceType == guestModel.ResourceBoard && (asked == guestModel.CapabilityComment || asked == guestModel.CapabilityEdit):
		return asked
	case asked == guestModel.CapabilityComment && (resourceType == guestModel.ResourceDoc || resourceType == guestModel.ResourceProject):
		return asked
	case asked == guestModel.CapabilityPost && resourceType == guestModel.ResourceChannel:
		return asked
	}
	return guestModel.CapabilityView
}

// BoardRole is a board link's role as asked, or ErrNoRole.
func BoardRole(asked string) (string, error) {
	switch asked = strings.TrimSpace(asked); asked {
	case guestModel.CapabilityView, guestModel.CapabilityComment, guestModel.CapabilityEdit:
		return asked, nil
	}
	return "", ErrNoRole
}

// hashSecret is a new password's or passcode's stored hash, named by noun in
// its refusal: SecretProblem's rule, then bcrypt.
func hashSecret(noun, secret string) (*string, error) {
	if why := SecretProblem(noun, secret); why != "" {
		return nil, &PublishInvalid{why}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	h := string(hash)
	return &h, nil
}

// BoardLinkChange is what a board's manager changes on a live link: each
// field nil keeps it; Password "" takes the password off.
type BoardLinkChange struct {
	Capability   *string
	Password     *string
	TTL          *time.Duration
	NeverExpires bool
}

// ChangeBoardLink changes a live board link's role, end or password, and
// ends the live sessions its guests have open, so each tab is authorised
// afresh: with the new role, or not at all (a new password needs typing).
func ChangeBoardLink(ctx context.Context, grantID uuid.UUID, in BoardLinkChange) (*guestModel.GuestGrant, error) {
	if !guestOn() {
		return nil, ErrGuestDisabled
	}
	g, err := grantByID(ctx, grantID)
	if err != nil {
		return nil, err
	}
	if g == nil || g.RevokedAt != nil || grantExpired(ctx, g) || g.ResourceType != guestModel.ResourceBoard {
		return nil, ErrNotABoardLink
	}
	c := guestModel.LinkChange{Capability: g.Capability, ExpiresAt: g.ExpiresAt, PasswordHash: g.PasswordHash}
	if in.Capability != nil {
		if c.Capability, err = BoardRole(*in.Capability); err != nil {
			return nil, err
		}
	}
	switch {
	case in.Password == nil:
	case *in.Password == "":
		c.NewUnlock, c.PasswordHash = g.PasswordHash != nil, nil
	default:
		if c.PasswordHash, err = hashSecret("password", *in.Password); err != nil {
			return nil, err
		}
		c.NewUnlock = true
	}
	if in.TTL != nil || in.NeverExpires {
		var ttl time.Duration
		if in.TTL != nil {
			ttl = *in.TTL
		}
		c.ExpiresAt = linkLifetime(ctx, KindLink, time.Now(), ttl, in.NeverExpires)
	}
	saved, err := changeLink(ctx, grantID, g, c)
	if err != nil {
		return nil, err
	}
	if saved == nil {
		// Not written: turned off or run out since, or changed by someone else
		// since it was read (the write is a compare-and-set).
		if now, err := grantByID(ctx, grantID); err == nil && now != nil && now.RevokedAt == nil && !grantExpired(ctx, now) && now.ResourceType == guestModel.ResourceBoard {
			return nil, ErrLinkChangedMeanwhile
		}
		return nil, ErrNotABoardLink
	}
	endLiveSessions(grantID)
	return saved, nil
}

// changeLink writes a link's change, endLiveSessions ends its guests' live
// sessions: seams for tests.
var (
	changeLink      = guestModel.ChangeLink
	endLiveSessions = EndLiveSessions
)

// OpenLocked is a password link's door, the docs' page unlock (C6) at a
// board link: a link without a password opens as it is (""); one with an
// unlock that still opens it answers that unlock; otherwise the password is
// tried from ip, counted against the address and the link first, and opens
// a fresh unlock.
func OpenLocked(ctx context.Context, g *guestModel.GuestGrant, password, unlock, ip string) (string, error) {
	if g == nil {
		return "", ErrInvalidGrant
	}
	if g.PasswordHash == nil {
		return "", nil
	}
	now := time.Now()
	if Unlocks(unlock, g, now) {
		return unlock, nil
	}
	if password == "" {
		return "", ErrPasswordNeeded
	}
	if !UnlockTryAllowed(ctx, helpers.RateKeyIP(ip), g) {
		return "", ErrTooManyTries
	}
	if !PasswordOpens(g, password) {
		return "", ErrWrongPassword
	}
	token, _ := MintUnlock(g, now)
	if token == "" {
		return "", errors.New("guest: the server has no signing secret")
	}
	return token, nil
}

// Unlocked is whether a guest's read of g may go ahead: g has no password,
// or unlock opens it now.
func Unlocked(g *guestModel.GuestGrant, unlock string) bool {
	return g != nil && (g.PasswordHash == nil || Unlocks(unlock, g, time.Now()))
}

// BoardGuest is a board link's guest writing on the board: the grant, and
// who they are on it (guest-<grant id>) under the name their token carries.
type BoardGuest struct {
	Grant *guestModel.GuestGrant
	ID    string
	Name  string
}

// CanComment is whether the guest's link lets them comment: comment or edit.
func (b BoardGuest) CanComment() bool {
	return b.Grant != nil && (b.Grant.Capability == guestModel.CapabilityComment || b.Grant.Capability == guestModel.CapabilityEdit)
}

// AuthorizeBoardGuest is the guest a live token names on board, checked as
// every live connection is (AuthorizeGuestCollab: the link on, not revoked,
// not ended, its password's version, the board live): the server's own
// record of who writes, never the client's.
func AuthorizeBoardGuest(ctx context.Context, bearerToken, board string) (BoardGuest, error) {
	g, claims, err := authorizeGuestCollab(ctx, bearerToken, guestModel.ResourceBoard, board)
	if err != nil {
		return BoardGuest{}, err
	}
	name := claims.Name
	if name == "" {
		name = callNamePrefix + "Guest"
	}
	return BoardGuest{Grant: g, ID: collabIdentity(g.Id), Name: name}, nil
}
