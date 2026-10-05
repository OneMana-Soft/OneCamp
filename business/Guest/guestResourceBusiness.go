package business

// Phase 2 guest access: scoped, expiring, READ-ONLY grants to a single doc,
// board, or table for an external person (client / contractor) with no OneCamp
// account.
//
// Design (production):
//   - A member who can access a resource mints a share link. Only the SHA-256
//     of the raw link token is stored (same model as Phase 1 meeting links).
//   - For docs/boards (live Yjs documents), the guest's browser connects to the
//     SAME collaboration service members use. The collab service delegates
//     authorization to the Go backend, exactly as it does for members; we add a
//     guest branch that validates the grant and joins the guest READ-ONLY (the
//     collab service rejects their writes). So guests see the live document, not
//     a stale snapshot, and the security decision stays in this one chokepoint.
//   - To reach the collab service the guest needs a bearer the service can pass
//     back to us. We mint a short-lived JWT (signed with JWT_SECRET, like the
//     member collab token) carrying a guest marker + the grant scope. The guest
//     authorize endpoint re-validates the grant on every (re)connect, so a
//     revoked or expired grant stops working immediately regardless of the JWT.
//   - Tables are Postgres-backed (not in the collab service); they are served by
//     a separate read endpoint gated by the same grant.
//
// A guest is NEVER written to the users table, so they can't appear in rosters,
// search, memory, mentions, or nudges — same invariant as Phase 1.

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// Default lifetime of a doc/board/table share link. Bounded so an
	// external grant can't live forever; the creator can pick a shorter TTL.
	resourceGrantTTL = 14 * 24 * time.Hour
	// Max lifetime an admin/member may request for a share link.
	maxResourceGrantTTL = 90 * 24 * time.Hour
	// Short lifetime of the guest collab JWT. The FE refetches it on every
	// (re)connect, and the guest authorize endpoint re-validates the grant
	// each time, so this only bounds how long a leaked JWT is usable before a
	// fresh grant re-check is forced.
	guestCollabTokenTTL = 30 * time.Minute
)

// ResourceGrant is the result of minting a doc/board/table share link.
type ResourceGrant struct {
	GrantID      uuid.UUID
	Token        string // raw link token, shown ONCE
	ResourceType string
	ResourceID   string
	Capability   string
	// ExpiresAt nil means the link lasts until it is revoked.
	ExpiresAt *time.Time
}

// GuestCollabClaims is the verified payload of a guest collab JWT.
type GuestCollabClaims struct {
	GrantID      uuid.UUID
	ResourceType string
	ResourceID   string
	Name         string
}

// grantExpired is the single Go-side definition of "past its expiry", kept
// beside the SQL predicate it has to agree with. A nil ExpiresAt is a grant that
// lasts until it is revoked, so it is never expired.
//
// One function rather than the comparison inlined at each check, because the two
// checks that exist got to disagree the moment the column became nullable, and
// the failure mode of disagreeing is a link that opens where it should not.
func grantExpired(g *guestModel.GuestGrant) bool {
	return g.ExpiresAt != nil && !g.ExpiresAt.After(time.Now())
}

// CreateResourceGrant mints a scoped, expiring grant for a doc/board/table.
// The CALLER must have already verified the creator may access the resource
// (the controller does this reusing the same access checks as the collab
// authorize endpoints), so this layer only enforces the workspace policy, the
// resource type, the capability, and the TTL bound. Returns the raw token once.
//
// capability is "view" or "comment". The comment capability is only meaningful
// for docs (the one resource with a structured comment thread); for board/table
// it is coerced to "view". An unknown capability is coerced to "view" too, so a
// grant can never be minted with broader rights than intended.
// neverExpires is a separate argument rather than a sentinel ttl because ttl<=0
// already means "use the default", and overloading it would turn a caller that
// simply forgot to set a duration into a permanent public link.
func CreateResourceGrant(ctx context.Context, creatorUUID uuid.UUID, resourceType, resourceID, capability string, ttl time.Duration, neverExpires bool) (*ResourceGrant, error) {
	if !settingsBusiness.GuestAccessEnabled() {
		return nil, ErrGuestDisabled
	}
	switch resourceType {
	case guestModel.ResourceDoc, guestModel.ResourceBoard, guestModel.ResourceTable, guestModel.ResourceChannel:
		// ok
	default:
		return nil, errors.New("unsupported resource type for guest access")
	}
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" {
		return nil, errors.New("missing resource id")
	}
	// Normalize capability: comment is doc-only; everything else is view.
	cap := guestModel.CapabilityView
	if capability == guestModel.CapabilityComment && resourceType == guestModel.ResourceDoc {
		cap = guestModel.CapabilityComment
	}
	if capability == guestModel.CapabilityPost && resourceType == guestModel.ResourceChannel {
		cap = guestModel.CapabilityPost
	}
	// The ceiling still applies to every link that HAS an expiry. Permanence is
	// only reachable by asking for it explicitly.
	if ttl <= 0 {
		ttl = resourceGrantTTL
	}
	if ttl > maxResourceGrantTTL {
		ttl = maxResourceGrantTTL
	}

	raw, err := randomToken()
	if err != nil {
		return nil, err
	}
	var expiresAt *time.Time
	if !neverExpires {
		at := time.Now().Add(ttl)
		expiresAt = &at
	}
	grantID, err := guestModel.CreateGrant(ctx, hashToken(raw), resourceType, resourceID, cap, creatorUUID, expiresAt)
	if err != nil {
		return nil, err
	}
	return &ResourceGrant{
		GrantID:      grantID,
		Token:        raw,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Capability:   cap,
		ExpiresAt:    expiresAt,
	}, nil
}

// maxGuestCommentLen bounds a single guest comment body (after HTML stripping).
// Generous for a review note, but capped so the isolated table can't be abused
// as bulk storage by a leaked comment link.
const maxGuestCommentLen = 4000

// GuestCommentView is one guest comment in display order, attributed to the
// badged guest identity only (never a member).
type GuestCommentView struct {
	ID        uuid.UUID
	GuestName string
	Body      string // plain text (HTML already stripped)
	CreatedAt time.Time
}

// CreateGuestDocComment posts a comment on a doc on behalf of a guest, after
// verifying the grant carries the comment capability for that doc. The body is
// HTML-stripped and length-capped at the boundary: a guest is an untrusted
// external user, so we store plain text only and never persist raw markup that
// would render inside an authenticated member session.
func CreateGuestDocComment(ctx context.Context, grant *guestModel.GuestGrant, displayName, body string) (*GuestCommentView, error) {
	if !settingsBusiness.GuestAccessEnabled() {
		return nil, ErrGuestDisabled
	}
	if grant == nil || grant.ResourceType != guestModel.ResourceDoc {
		return nil, ErrInvalidGrant
	}
	if grant.Capability != guestModel.CapabilityComment {
		return nil, ErrForbidden
	}
	// Strip ALL HTML, collapse, trim, and length-cap. Stored as plain text.
	plain, err := sanitizeGuestCommentBody(body)
	if err != nil {
		return nil, err
	}
	name := sanitizeGuestName(displayName)
	if name == "" {
		name = "Guest"
	}
	id, createdAt, err := guestModel.CreateGuestComment(ctx, grant.Id, grant.ResourceID, name, plain)
	if err != nil {
		return nil, err
	}
	return &GuestCommentView{ID: id, GuestName: name, Body: plain, CreatedAt: createdAt}, nil
}

// sanitizeGuestCommentBody is the security boundary for guest-submitted comment
// text: it strips ALL HTML (guest input renders inside authenticated member
// sessions, so raw markup would be a stored-XSS vector), trims, and length-caps
// to maxGuestCommentLen. Returns an error for empty/whitespace-only input.
func sanitizeGuestCommentBody(body string) (string, error) {
	plain := strings.TrimSpace(helpers.RemoveHTMLTags(body))
	if plain == "" {
		return "", errEmptyComment
	}
	if len(plain) > maxGuestCommentLen {
		plain = plain[:maxGuestCommentLen]
	}
	return plain, nil
}

// errEmptyComment is returned when a guest comment is empty after sanitization.
var errEmptyComment = errors.New("empty comment")

// ListGuestDocComments returns the live guest comments for a doc in thread
// order. Used both by the guest viewer and to merge into the member comment
// list. Bodies are plain text; callers escape on render.
func ListGuestDocComments(ctx context.Context, docUUID string) ([]*GuestCommentView, error) {
	rows, err := guestModel.ListGuestCommentsByDoc(ctx, docUUID)
	if err != nil {
		return nil, err
	}
	out := make([]*GuestCommentView, 0, len(rows))
	for _, c := range rows {
		out = append(out, &GuestCommentView{
			ID:        c.Id,
			GuestName: c.GuestName,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
	return out, nil
}

// ValidateResourceGrant resolves a raw link token to its active grant for the
// expected resource type, enforcing the workspace policy. Returns a sentinel
// error (mapped to a uniform "not available" upstream) when access is not
// available for any reason — no oracle.
func ValidateResourceGrant(ctx context.Context, rawToken, expectedType string) (*guestModel.GuestGrant, error) {
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
	if g == nil || g.ResourceType != expectedType {
		return nil, ErrInvalidGrant
	}
	return g, nil
}

// ValidateCollabGrant resolves a raw link token to an active grant for a LIVE
// collaborative resource (doc or board), enforcing the workspace policy. Used
// by the guest collab-token endpoint, which doesn't know the type in advance.
func ValidateCollabGrant(ctx context.Context, rawToken string) (*guestModel.GuestGrant, error) {
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
	if g == nil || (g.ResourceType != guestModel.ResourceDoc && g.ResourceType != guestModel.ResourceBoard) {
		return nil, ErrInvalidGrant
	}
	return g, nil
}

// IssueGuestCollabToken mints a short-lived JWT the guest's browser hands to the
// collaboration service. It is signed with JWT_SECRET (same as the member
// collab token) and carries a guest marker so the collab service routes it to
// the guest authorize endpoint, plus the grant scope so authorization can't be
// widened by a tampered document name. Display name is sanitized.
func IssueGuestCollabToken(ctx context.Context, grant *guestModel.GuestGrant, displayName string) (string, error) {
	if grant == nil {
		return "", ErrInvalidGrant
	}
	name := sanitizeGuestName(displayName)
	if name == "" {
		name = "Guest"
	}
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return "", errors.New("server auth not configured")
	}
	now := time.Now()
	claims := jwt.MapClaims{
		// `sub` lives in the reserved guest namespace so it can never collide
		// with a member UUID identity in the collab awareness layer.
		"sub":   "guest-" + grant.Id.String(),
		"name":  "Guest: " + name,
		"guest": true,
		"gid":   grant.Id.String(),
		"rt":    grant.ResourceType,
		"rid":   grant.ResourceID,
		"iat":   now.Unix(),
		"exp":   now.Add(guestCollabTokenTTL).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Guest/IssueGuestCollabToken sign err: %+v", err)
		return "", err
	}
	return signed, nil
}

// AuthorizeGuestCollab verifies a guest collab JWT for a requested resource and
// re-validates the underlying grant. Called by the collaboration service on
// every (re)connect, so revocation/expiry/policy-off take effect immediately
// regardless of the JWT's own (short) lifetime. kind is "doc" or "board" and
// resourceID is the document id the guest is trying to open; BOTH must match
// the grant baked into the JWT, so a valid guest token for one doc can't open
// another. Returns the validated grant on success.
func AuthorizeGuestCollab(ctx context.Context, bearerToken, kind, resourceID string) (*guestModel.GuestGrant, error) {
	if !settingsBusiness.GuestAccessEnabled() {
		return nil, ErrGuestDisabled
	}
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return nil, errors.New("server auth not configured")
	}
	claims, err := parseGuestCollabClaims(strings.TrimSpace(bearerToken), secret)
	if err != nil {
		return nil, ErrInvalidGrant
	}
	// The JWT's baked-in scope must match BOTH the resource kind and the exact
	// id the collab service is asking about — no widening.
	if claims.ResourceType != kind || claims.ResourceID != strings.TrimSpace(resourceID) {
		return nil, ErrInvalidGrant
	}
	// Re-validate the grant in the DB: active, not revoked, not expired, and
	// still the same resource. This is what makes revocation immediate.
	g, err := guestModel.GetByID(ctx, claims.GrantID)
	if err != nil {
		return nil, err
	}
	if g == nil || g.RevokedAt != nil || grantExpired(g) {
		return nil, ErrInvalidGrant
	}
	if g.ResourceType != kind || g.ResourceID != strings.TrimSpace(resourceID) {
		return nil, ErrInvalidGrant
	}
	return g, nil
}

// parseGuestCollabClaims verifies the JWT signature + expiry and extracts the
// guest scope. Rejects any token that is not a guest token.
func parseGuestCollabClaims(tokenString, secret string) (*GuestCollabClaims, error) {
	if tokenString == "" {
		return nil, ErrInvalidGrant
	}
	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil || !token.Valid {
		return nil, ErrInvalidGrant
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, ErrInvalidGrant
	}
	if g, _ := claims["guest"].(bool); !g {
		return nil, ErrInvalidGrant
	}
	gidStr, _ := claims["gid"].(string)
	gid, err := uuid.Parse(gidStr)
	if err != nil {
		return nil, ErrInvalidGrant
	}
	rt, _ := claims["rt"].(string)
	rid, _ := claims["rid"].(string)
	name, _ := claims["name"].(string)
	if rt == "" || rid == "" {
		return nil, ErrInvalidGrant
	}
	return &GuestCollabClaims{GrantID: gid, ResourceType: rt, ResourceID: rid, Name: name}, nil
}
