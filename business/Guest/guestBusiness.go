// Package business (Guest) implements scoped, expiring guest access.
//
// A guest is a least-privilege principal scoped to exactly one resource for a
// bounded time. Guests get NO OneCamp session/JWT and NO API access — only, in
// Phase 1, a single-room LiveKit media token. Guests are never written to the
// users table, so they cannot appear in rosters, search, memory, mentions, or
// nudges.
//
// Token model (mirrors API tokens / webhook secrets): the raw link token is a
// 32-byte URL-safe random string shown ONCE in the share link; only its
// SHA-256 is stored. A grant is usable iff found-by-hash AND not revoked AND
// not expired AND the workspace guest-access policy is ON.
package business

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// Tunables.
const (
	// How long a guest meeting link stays usable.
	meetingGrantTTL = 12 * time.Hour
	// LiveKit join-window for a guest token (the call session persists once
	// connected; this only bounds the connect window and re-joins).
	guestTokenTTL = 4 * time.Hour
	// Instant-meeting room names use this prefix so the call webhook router
	// can distinguish them from channel/DM/group rooms. Keep in sync with the
	// LiveKit webhook handler (roomBelongsToThisInstance / broadcastCallStop).
	MeetingRoomPrefix = "meet-"
	// Display-name bounds for a guest.
	maxGuestNameLen = 40
)

// Sentinel errors. The public controller maps ALL of these to a single,
// indistinguishable "not available" response so a guest can't tell apart
// disabled / invalid / expired / revoked (no oracle).
var (
	ErrGuestDisabled = errors.New("guest access is disabled")
	ErrInvalidGrant  = errors.New("invalid or expired guest link")
	ErrInvalidName   = errors.New("a display name is required")
	// ErrForbidden is returned when a valid grant lacks the capability for the
	// attempted action (e.g. a view-only grant trying to comment).
	ErrForbidden = errors.New("this guest link does not allow that action")
)

// IsUnavailable reports whether err is an answer about the link or what it
// opens (guest access off; the link invalid, expired or revoked; the thing
// gone, or outside the link) rather than the server failing to answer. The
// first gets the one uniform "not available", so there is no oracle; the
// second a "try again", so a guest's page keeps retrying instead of telling a
// client their link has stopped working because a database didn't answer.
func IsUnavailable(err error) bool {
	return errors.Is(err, ErrGuestDisabled) || errors.Is(err, ErrInvalidGrant) ||
		errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden)
}

// readFailed reports whether a read failed, as opposed to answering that what
// it looked for isn't there (notFound is that read's own answer). A store that
// didn't answer says nothing about the link, so the read's error is kept and
// becomes the 503 a guest's page retries, never ErrNotFound's dead link.
func readFailed(err, notFound error) bool {
	return err != nil && !errors.Is(err, notFound)
}

// InstantMeeting is the result of starting a guest-shareable meeting.
type InstantMeeting struct {
	Room       string    // LiveKit room name ("meet-<uuid>")
	HostToken  string    // member LiveKit token (room admin)
	GuestToken string    // raw link token (shown once; not stored raw)
	GrantID    uuid.UUID // grant row id (for revoke / display)
	ExpiresAt  time.Time
}

// CreateInstantMeeting starts a fresh meeting room, mints the host's member
// token (room admin), and creates a guest grant whose raw link token is
// returned once. The caller (controller) builds the shareable URL from the
// raw token and records the audit entry.
func CreateInstantMeeting(ctx context.Context, host *dgraphStruct.DgraphUser, audioEnabled, videoEnabled bool) (*InstantMeeting, error) {
	if host == nil {
		return nil, errors.New("missing host info")
	}
	hostUUID, err := uuid.Parse(host.Uuid)
	if err != nil {
		return nil, errors.New("invalid host id")
	}

	room := MeetingRoomPrefix + uuid.NewString()

	// Create the room and mint the host token (room admin so the host can
	// manage the call). Reuses the same path members use for channel/DM calls.
	hostToken, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, host, true, audioEnabled, videoEnabled)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Guest/CreateInstantMeeting host token err: %+v", err)
		return nil, err
	}

	raw, err := randomToken()
	if err != nil {
		return nil, err
	}
	// Always bounded. A meeting link is for one meeting, and CreateGrant's nil
	// case (never expires) has no meaning for a room that stops existing.
	expiresAt := time.Now().Add(meetingGrantTTL)
	grantID, err := guestModel.CreateGrant(ctx, hashToken(raw), guestModel.ResourceMeeting, room, guestModel.CapabilityJoin, hostUUID, &expiresAt)
	if err != nil {
		return nil, err
	}

	return &InstantMeeting{
		Room:       room,
		HostToken:  hostToken,
		GuestToken: raw,
		GrantID:    grantID,
		ExpiresAt:  expiresAt,
	}, nil
}

// ValidateMeetingGrant resolves a raw guest link token to its active meeting
// grant, enforcing the workspace policy. Returns a sentinel error (mapped to a
// uniform response upstream) when access is not available for any reason, and
// a read that failed as it is.
func ValidateMeetingGrant(ctx context.Context, rawToken string) (*guestModel.GuestGrant, error) {
	if !settingsBusiness.GuestAccessEnabled() {
		return nil, ErrGuestDisabled
	}
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return nil, ErrInvalidGrant
	}
	g, err := guestModel.GetActiveByHash(ctx, hashToken(rawToken))
	if err != nil {
		return nil, err
	}
	if g == nil || g.ResourceType != guestModel.ResourceMeeting {
		return nil, ErrInvalidGrant
	}
	return g, nil
}

// IssueGuestMeetingToken mints a single-room, short-lived LiveKit token for a
// validated meeting grant. The guest identity lives in the reserved `guest-`
// namespace so it can never collide with a member UUID identity or the
// transcription agent. The display name is sanitized and rendered with a
// "Guest:" prefix so the UI badges it unambiguously.
func IssueGuestMeetingToken(ctx context.Context, grant *guestModel.GuestGrant, displayName string, audioEnabled, videoEnabled bool) (token string, room string, err error) {
	name := sanitizeGuestName(displayName)
	if name == "" {
		return "", "", ErrInvalidName
	}
	// Re-affirm the policy at issue time (defense in depth; the link could be
	// validated then the policy flipped off before join).
	if !settingsBusiness.GuestAccessEnabled() {
		return "", "", ErrGuestDisabled
	}

	suffix, err := randomShort()
	if err != nil {
		return "", "", err
	}
	identity := "guest-" + grant.Id.String() + "-" + suffix

	token, err = livekitInit.LiveKitService.GenerateGuestToken(grant.ResourceID, identity, "Guest: "+name, guestTokenTTL, audioEnabled, videoEnabled)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Guest/IssueGuestMeetingToken err: %+v", err)
		return "", "", err
	}
	return token, grant.ResourceID, nil
}

// ListActiveGrants returns active grants for the admin view.
func ListActiveGrants(ctx context.Context) ([]*guestModel.GuestGrant, error) {
	return guestModel.ListActive(ctx)
}

// RevokeGrant revokes a grant by id (admin-gated upstream).
func RevokeGrant(ctx context.Context, id uuid.UUID) error {
	return guestModel.Revoke(ctx, id)
}

// IsMeetingRoom reports whether a LiveKit room name is an instant-meeting room
// (the `meet-` namespace). Used by the call webhook router to distinguish
// instant meetings from channel/DM/group rooms.
func IsMeetingRoom(roomName string) bool {
	return strings.HasPrefix(roomName, MeetingRoomPrefix)
}

// MeetingRoomExists reports whether an instant-meeting room belongs to this
// instance (a guest grant row exists for it). Used by the webhook router's
// multi-tenant ownership check.
func MeetingRoomExists(ctx context.Context, roomName string) (bool, error) {
	return guestModel.ResourceExists(ctx, roomName)
}

// --- helpers ---

// randomToken returns a 32-byte URL-safe random string (the raw link token).
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// randomShort returns a short random suffix for a guest identity.
func randomShort() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken returns the raw SHA-256 bytes of a token (stored as bytea).
func hashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// SanitizeGuestName is the exported wrapper over sanitizeGuestName, used by
// controllers that need to record a safe, content-free guest display name for
// audit (R4.4) without resolving it to any member.
func SanitizeGuestName(in string) string { return sanitizeGuestName(in) }

// sanitizeGuestName trims, strips control characters, collapses whitespace, and
// length-caps a guest-entered display name. Returns "" when nothing usable
// remains (caller rejects).
func sanitizeGuestName(in string) string {
	in = strings.TrimSpace(in)
	if in == "" {
		return ""
	}
	var b strings.Builder
	prevSpace := false
	for _, r := range in {
		// Treat any whitespace (including newlines/tabs) as a single space,
		// then strip remaining non-space control characters.
		if unicode.IsSpace(r) {
			if prevSpace {
				continue
			}
			prevSpace = true
			b.WriteRune(' ')
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len([]rune(out)) > maxGuestNameLen {
		out = string([]rune(out)[:maxGuestNameLen])
		out = strings.TrimSpace(out)
	}
	return out
}
