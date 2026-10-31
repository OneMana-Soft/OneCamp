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
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/pgexec"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

// Tunables.
const (
	// How long a guest meeting link stays usable.
	meetingGrantTTL = 12 * time.Hour
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
	MeetingID  string    // the <uuid>: the meeting's stable page and endpoints
	HostToken  string    // member LiveKit token (room admin)
	GuestToken string    // raw link token (shown once; not stored raw)
	GrantID    uuid.UUID // grant row id (for revoke / display)
	ExpiresAt  time.Time
}

// ErrNotHost is a member asking for an instant meeting that someone else
// started.
var ErrNotHost = errors.New("only the person who started this meeting can do that")

// ErrNoMeeting is an id no instant meeting ever had. It is also ErrNotFound,
// so a caller that only knows "not there" still answers it; the meeting's own
// page tells it apart from one that has ended (F-54), which said "This
// meeting has ended" about a meeting that never was.
var ErrNoMeeting = fmt.Errorf("no meeting has this id: %w", ErrNotFound)

// CreateInstantMeeting starts a fresh meeting room, mints the host's member
// token (room admin), and creates a guest grant whose raw link token is
// returned once. The caller (controller) builds the shareable URL from the
// raw token and records the audit entry.
func CreateInstantMeeting(ctx context.Context, host *dgraphStruct.DgraphUser, language string) (*InstantMeeting, error) {
	if host == nil {
		return nil, errors.New("missing host info")
	}
	hostUUID, err := uuid.Parse(host.Uuid)
	if err != nil {
		return nil, errors.New("invalid host id")
	}

	meetingID := uuid.NewString()
	room := MeetingRoomPrefix + meetingID

	// Create the room and mint the host token (room admin so the host can
	// manage the call). Reuses the same path members use for channel/DM calls.
	hostToken, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, host, true, language)
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
		MeetingID:  meetingID,
		HostToken:  hostToken,
		GuestToken: raw,
		GrantID:    grantID,
		ExpiresAt:  expiresAt,
	}, nil
}

// IssueGuestMeetingToken mints a single-room, short-lived LiveKit token for
// the call a door opens (a meeting's link, a room's address, a call's
// invite). The guest identity lives in the reserved `guest-` namespace so it
// can never collide with a member UUID identity or the transcription agent.
// The display name is sanitized and rendered with a "Guest:" prefix so the UI
// badges it unambiguously.
func IssueGuestMeetingToken(ctx context.Context, door Door, displayName, language string) (token string, room string, err error) {
	grant := door.Grant
	name := sanitizeGuestName(displayName)
	if name == "" {
		return "", "", ErrInvalidName
	}
	// Re-affirm the policy at issue time (defense in depth; the link could be
	// validated then the policy flipped off before join).
	if !settingsBusiness.GuestAccessEnabled() {
		return "", "", ErrGuestDisabled
	}
	// A meeting its host locked lets nobody new in, and a guest's every join
	// is someone new (liveKitBusiness.ErrCallLocked, said in its own words).
	if err := liveKitBusiness.GuestMayEnter(ctx, door.Room); err != nil {
		return "", "", err
	}
	// What the people running the call hold for everyone else holds a guest too.
	sources, err := liveKitBusiness.SourcesAtDoor(ctx, door.Room, nil, false)
	if err != nil {
		return "", "", err
	}

	suffix, err := randomShort()
	if err != nil {
		return "", "", err
	}
	identity := guestIdentityPrefix(grant.Id.String()) + suffix

	token, err = livekitInit.LiveKitService.GenerateGuestToken(door.Room, identity, callNamePrefix+name, language, sources)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Guest/IssueGuestMeetingToken err: %+v", err)
		return "", "", err
	}
	return token, door.Room, nil
}

// AdminGrant is a live grant as the admin's list shows it, with the name of
// the member who made it: a room reads "Maya Chen's room", where a column of
// "Personal room" rows told an admin nothing.
type AdminGrant struct {
	*guestModel.GuestGrant
	// CreatedByName is left out for a member who has gone or whose name
	// couldn't be read.
	CreatedByName string `json:"created_by_name,omitempty"`
}

// activeGrants and displayNames are the admin list's two reads: seams for tests.
var (
	activeGrants = guestModel.ListActive
	displayNames = userModels.DisplayNamesByUUIDs
)

// ListActiveGrants is every live grant for the admin view, newest first, each
// with its maker's name: one query for the grants and one for the names.
// Names that can't be read leave the rows standing, so an admin can still
// revoke.
func ListActiveGrants(ctx context.Context) ([]AdminGrant, error) {
	grants, err := activeGrants(ctx)
	if err != nil {
		return nil, err
	}
	makers := make([]uuid.UUID, 0, len(grants))
	for _, g := range grants {
		makers = append(makers, g.CreatedBy)
	}
	names, err := displayNames(ctx, makers)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Guest/ListActiveGrants names err: %+v", err)
	}
	out := make([]AdminGrant, 0, len(grants))
	for _, g := range grants {
		out = append(out, AdminGrant{GuestGrant: g, CreatedByName: names[g.CreatedBy]})
	}
	return out, nil
}

// RevokeGrant revokes a grant by id, for an admin's settings and a link's
// maker or sharer in its dialog (each checked upstream). A doc's or board's
// guests lose their live view at once.
func RevokeGrant(ctx context.Context, id uuid.UUID) error {
	if err := revoke(ctx, id); err != nil {
		return err
	}
	EndLiveSessions(id)
	return nil
}

// RevokeGrants revokes each of these grants as RevokeGrant does, and ends the
// live views their guests have open in one call: a doc's links turned off as
// it is made private. It answers which it revoked; one it can't is logged.
func RevokeGrants(ctx context.Context, ids []uuid.UUID) []uuid.UUID {
	var done []uuid.UUID
	for _, id := range ids {
		if err := revoke(ctx, id); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Guest/RevokeGrants %s: %+v", id, err)
			continue
		}
		done = append(done, id)
	}
	EndLiveSessions(done...)
	return done
}

// revoke turns a grant off. A meeting link's guests still in the meeting are
// put out of it: revoking used to mark the link revoked and nothing else, and
// their tokens kept connecting.
func revoke(ctx context.Context, id uuid.UUID) error {
	if err := guestModel.Revoke(ctx, id); err != nil {
		return err
	}
	// The call it opens (a meeting's, or a call's while it goes on): its
	// guests go, and so do the members who came in by it as themselves.
	if g, err := grantByID(ctx, id); err == nil && g != nil {
		if door, err := DoorOf(ctx, g); err == nil {
			removeGuestsOf(ctx, door.Room, map[string]bool{id.String(): true})
		}
	}
	putOutMembersOf(ctx, []uuid.UUID{id})
	return nil
}

// collabIdentity is who a guest is on the doc or board their link opens live:
// guest-<grant id>, their collab token's sub, never a member's uuid.
func collabIdentity(grantID uuid.UUID) string {
	return "guest-" + grantID.String()
}

// EndLiveSessions closes the live sockets that guests opened by these links,
// so a link turned off stops showing its live edits at once: the reconnect
// asks AuthorizeGuestCollab, which refuses it. Only a doc's or a board's link
// opens one. In the background; nothing here fails the change.
func EndLiveSessions(grantIDs ...uuid.UUID) {
	ids := make([]string, 0, len(grantIDs))
	for _, id := range grantIDs {
		ids = append(ids, collabIdentity(id))
	}
	collab.ClosePeopleLater(ids...)
}

// endLinkSessions ends the live sessions of every live doc and board link.
func endLinkSessions(ctx context.Context) {
	grants, err := activeGrants(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Guest/endLinkSessions could not list the links: %+v", err)
		return
	}
	var ids []uuid.UUID
	for _, g := range grants {
		if g.ResourceType == guestModel.ResourceDoc || g.ResourceType == guestModel.ResourceBoard {
			ids = append(ids, g.Id)
		}
	}
	EndLiveSessions(ids...)
}

// callNamePrefix leads a guest's name in a call, so the UI badges it.
const callNamePrefix = "Guest: "

// NameInCall is the name a guest gave, from their name in a call. Pure.
func NameInCall(callName string) string {
	return strings.TrimPrefix(callName, callNamePrefix)
}

// guestIdentityPrefix starts the call identity of every guest who joins from
// one grant: "guest-<grant id>-<random>". Pure.
func guestIdentityPrefix(grantID string) string {
	return "guest-" + grantID + "-"
}

// grantOfGuest answers which grant a guest's call identity came from, "" for
// anything that is not a guest's identity. Pure.
func grantOfGuest(identity string) string {
	rest, ok := strings.CutPrefix(identity, "guest-")
	if !ok || len(rest) < 37 || rest[36] != '-' {
		return ""
	}
	if _, err := uuid.Parse(rest[:36]); err != nil {
		return ""
	}
	return rest[:36]
}

// removeGuestsOf puts out of a meeting's call every guest who joined from one
// of these grants (all of its guests when grants is nil).
func removeGuestsOf(ctx context.Context, room string, grants map[string]bool) {
	liveKitBusiness.RemoveGuestsWhere(ctx, room, liveKitBusiness.RemovedLinkRevoked, func(identity string) bool {
		return grants == nil || grants[grantOfGuest(identity)]
	})
}

// GuestMayJoin reports whether a guest's call identity still has a live link
// to the call in this room: guest access on, the link neither revoked nor
// expired, and opening this room now (a meeting's link its meeting, a call's
// invite its call while it goes on).
func GuestMayJoin(ctx context.Context, room, identity string) (bool, error) {
	if !guestOn() {
		return false, nil
	}
	id, err := uuid.Parse(grantOfGuest(identity))
	if err != nil {
		return false, nil
	}
	g, err := grantByID(ctx, id)
	if err != nil {
		return false, err
	}
	if g == nil || g.RevokedAt != nil || grantExpired(ctx, g) {
		return false, nil
	}
	// A personal room's guest, only while rooms are on: its address stops
	// answering at once, and a token minted before must not bring them back.
	if guestModel.IsStanding(g) && !roomsOn() {
		return false, nil
	}
	opens, err := doorRoom(ctx, g)
	if errors.Is(err, ErrInvalidGrant) {
		return false, nil
	}
	return err == nil && opens == room, err
}

// RemoveMeetingGuests puts every guest out of one meeting's call, and the
// members who came in by its links as themselves: a personal room being
// closed (business/CallRoom), whose links all end with it.
func RemoveMeetingGuests(ctx context.Context, room string) {
	removeGuestsOf(ctx, room, nil)
	admitted, err := admittedIn(ctx, room)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Guest/RemoveMeetingGuests could not list who came in by link: %+v", err)
		return
	}
	putOutLinkMembers(ctx, admitted)
}

// RemoveRoomGuests puts every guest who came in at a personal room's address
// out of the room's call, for rooms being turned off (as RemoveAllLinkGuests
// does for guest access): the addresses stop answering at once, and nothing
// put out the guests already in. Only rooms with a call on are asked.
func RemoveRoomGuests(ctx context.Context) {
	standing, err := guestModel.ListLiveStanding(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Guest/RemoveRoomGuests could not list the rooms: %+v", err)
		return
	}
	byRoom := map[string]map[string]bool{}
	ids := make([]uuid.UUID, 0, len(standing))
	for _, g := range standing {
		if byRoom[g.ResourceID] == nil {
			byRoom[g.ResourceID] = map[string]bool{}
		}
		byRoom[g.ResourceID][g.Id.String()] = true
		ids = append(ids, g.Id)
	}
	if len(byRoom) > 0 {
		removeMeetingGuests(ctx, byRoom)
		// And the members who came in at a room's address as themselves.
		putOutMembersOf(ctx, ids)
	}
}

// RemoveAllLinkGuests puts everyone a link let in out of every call, for
// guest access being turned off: every guest (a meeting's, a room's, a call's
// invite's), and every member who came in by a link as themselves and has no
// other way in. The policy is re-read when someone joins, and nothing put out
// the ones already in.
func RemoveAllLinkGuests(ctx context.Context) {
	// Their open docs and boards first: a link's socket was authorised when it
	// connected, and the policy is read again only on a reconnect.
	endLinkSessions(ctx)
	removeMeetingGuests(ctx, nil)
	admitted, err := admittedInLiveCalls(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Guest/RemoveAllLinkGuests could not list who came in by link: %+v", err)
		return
	}
	putOutLinkMembers(ctx, admitted)
}

// removeMeetingGuests puts out of every meeting with a call on the guests of
// the grants only names for its room. With only nil it is every guest in
// every call, a conversation's included (a call's invite lets guests in
// there): one list of the calls, then one ask of the call server per call, as
// it has none that reaches across calls.
func removeMeetingGuests(ctx context.Context, only map[string]map[string]bool) {
	prefix := MeetingRoomPrefix
	if only == nil {
		prefix = ""
	}
	rooms, err := liveKitBusiness.CallRoomsWithPrefix(ctx, prefix)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Guest/removeMeetingGuests could not list meetings: %+v", err)
		return
	}
	for _, room := range rooms {
		switch grants := only[room]; {
		case only == nil:
			removeGuestsOf(ctx, room, nil)
		case grants != nil:
			removeGuestsOf(ctx, room, grants)
		}
	}
}

// MeetingRoom is an instant meeting's room, from its id. ok is false for
// anything that is not a meeting id. Pure.
func MeetingRoom(meetingID string) (string, bool) {
	id, err := uuid.Parse(strings.TrimSpace(meetingID))
	if err != nil {
		return "", false
	}
	return MeetingRoomPrefix + id.String(), true
}

// MeetingHost answers who started an instant meeting (the creator of its
// first link), whether or not the meeting is still on.
func MeetingHost(ctx context.Context, room string) (uuid.UUID, bool, error) {
	return guestModel.ResourceCreator(ctx, guestModel.ResourceMeeting, room)
}

// Hosted is a meeting as its host sees it: when its last link with an end
// stops working (zero while none has one), and, for a personal room, its
// address's slug ("" for an instant meeting).
type Hosted struct {
	Until time.Time
	Slug  string
}

// HostedMeeting answers when an instant meeting's link stops working, or a
// personal room's address, for its host only. ErrNoMeeting for a room no
// meeting ever had (every meeting starts with a link, and links are kept
// after they stop working); ErrNotFound once no link to it is live (the
// meeting is over); ErrNotHost for anyone but the person who started it.
func HostedMeeting(ctx context.Context, room string, host *dgraphStruct.DgraphUser) (Hosted, error) {
	if host == nil {
		return Hosted{}, ErrNotHost
	}
	creator, ok, err := MeetingHost(ctx, room)
	if err != nil {
		return Hosted{}, err
	}
	if !ok {
		return Hosted{}, ErrNoMeeting
	}
	if creator.String() != host.Uuid {
		return Hosted{}, ErrNotHost
	}
	live, err := guestModel.ListActiveForResource(ctx, guestModel.ResourceMeeting, room)
	if err != nil {
		return Hosted{}, err
	}
	var h Hosted
	for _, g := range live {
		if g.ExpiresAt != nil && g.ExpiresAt.After(h.Until) {
			h.Until = *g.ExpiresAt
		}
	}
	if len(live) == 0 {
		return Hosted{}, ErrNotFound
	}
	if room, _ := guestModel.StandingOf(live); room != nil {
		h.Slug = *room.Slug
	}
	return h, nil
}

// RejoinMeeting mints the host's token again for an instant meeting they
// started. Its token and link lived only in the page that started it, so
// leaving or reloading that page lost both for good, and the guests stayed in
// a meeting nobody could host.
func RejoinMeeting(ctx context.Context, room string, host *dgraphStruct.DgraphUser, language string) (string, error) {
	if _, err := HostedMeeting(ctx, room, host); err != nil {
		return "", err
	}
	token, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, host, true, language)
	return token, err
}

// NewMeetingLink makes another guest link to an instant meeting, for its host:
// a link is shown once and only its hash is kept, so a host who lost the page
// lost the link. With revokePrevious the links before it stop working and the
// guests who came in by them are put out of the call: how a host keeps out
// someone they removed.
func NewMeetingLink(ctx context.Context, room string, host *dgraphStruct.DgraphUser, revokePrevious bool) (*InstantMeeting, error) {
	if _, err := HostedMeeting(ctx, room, host); err != nil {
		return nil, err
	}
	hostUUID, err := uuid.Parse(host.Uuid)
	if err != nil {
		return nil, ErrNotHost
	}
	// The links to revoke are read first, and revoked only once the new one
	// exists: a failure in between must not leave the meeting with no live
	// link, which ends it for the host too (CALL-BUG-17).
	// A personal room's address is not one of them: it ends only by the
	// room's own controls (business/CallRoom), an admin, or rooms turned off.
	var old []*guestModel.GuestGrant
	if revokePrevious {
		live, err := guestModel.ListActiveForResource(ctx, guestModel.ResourceMeeting, room)
		if err != nil {
			return nil, err
		}
		_, old = guestModel.StandingOf(live)
	}
	raw, err := randomToken()
	if err != nil {
		return nil, err
	}
	expiresAt := time.Now().Add(meetingGrantTTL)
	grantID, err := guestModel.CreateGrant(ctx, hashToken(raw), guestModel.ResourceMeeting, room, guestModel.CapabilityJoin, hostUUID, &expiresAt)
	if err != nil {
		return nil, err
	}
	if len(old) > 0 {
		// A link that stopped working puts its guests out even when a later
		// one fails to stop: the host is told it failed and tries again, and
		// those links aren't live any more for that retry to find.
		revoked := map[string]bool{}
		var failed error
		for _, g := range old {
			if err := guestModel.Revoke(ctx, g.Id); err != nil && !errors.Is(err, sql.ErrNoRows) {
				failed = err
				break
			}
			revoked[g.Id.String()] = true
		}
		removeGuestsOf(ctx, room, revoked)
		if failed != nil {
			return nil, failed
		}
	}
	return &InstantMeeting{
		Room:       room,
		MeetingID:  strings.TrimPrefix(room, MeetingRoomPrefix),
		GuestToken: raw,
		GrantID:    grantID,
		ExpiresAt:  expiresAt,
	}, nil
}

// IsMeetingRoom reports whether a LiveKit room name is an instant-meeting room
// (the `meet-` namespace). Used by the call webhook router to distinguish
// instant meetings from channel/DM/group rooms.
func IsMeetingRoom(roomName string) bool {
	return strings.HasPrefix(roomName, MeetingRoomPrefix)
}

// --- helpers ---

// randomToken is a fresh raw link token (helpers.NewLinkSecret).
func randomToken() (string, error) {
	raw, _, err := helpers.NewLinkSecret()
	return raw, err
}

// Seams for tests: making a grant, and a transaction.
var (
	createGrant = guestModel.CreateGrant
	inTx        = pgexec.InTx
)

// NewRoom makes a member's personal meeting room's link (calls CALLS-2) and
// gives it its address, in one transaction (agreed with DOCS-LEAD-2, 20
// Oct): a join link to the room that never ends (linkLifetime of KindRoom,
// which the workspace's link cap leaves alone, as a room is standing), at
// its owner's name when that is free. A room that can't take an address
// isn't made, so under a cap there is never a never-ending link with no
// address. Its token is never shown: a room is opened at its address.
func NewRoom(ctx context.Context, room string, owner uuid.UUID) (*guestModel.GuestGrant, error) {
	return newRoom(ctx, room, owner, "")
}

// NewEventRoom makes an event's call's link (calendar B11) as NewRoom makes a
// personal room's, at an address of the event's title and a few random letters
// ("weekly-sync-3f9a1c2e"), never its bare words: a title is no one's name, and
// an event's call shouldn't be found from its title alone.
func NewEventRoom(ctx context.Context, room string, owner uuid.UUID, title string) (*guestModel.GuestGrant, error) {
	if normaliseSlug(title) == "" {
		title = "call"
	}
	return newRoom(ctx, room, owner, slugFor(title))
}

// newRoom is NewRoom at slug, or at the room's fresh address for "".
func newRoom(ctx context.Context, room string, owner uuid.UUID, slug string) (*guestModel.GuestGrant, error) {
	if !guestOn() {
		return nil, ErrGuestDisabled
	}
	if !roomsAllowed() {
		return nil, ErrRoomsOff
	}
	raw, err := randomToken()
	if err != nil {
		return nil, err
	}
	var g *guestModel.GuestGrant
	err = inTx(ctx, func(ctx context.Context) error {
		id, err := createGrant(ctx, hashToken(raw), guestModel.ResourceMeeting, room, guestModel.CapabilityJoin, owner, linkLifetime(ctx, KindRoom, time.Now(), 0, true))
		if err != nil {
			return err
		}
		g, err = PublishLink(ctx, owner, id, PublishSettings{Slug: slug})
		return err
	})
	if err != nil {
		return nil, err
	}
	return g, nil
}

// randomShort returns a short random suffix for a guest identity.
func randomShort() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken is the hash a token is stored as (bytea) and found by.
func hashToken(raw string) []byte { return helpers.LinkSecretHash(raw) }

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
