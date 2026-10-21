package business

// One external-access model (docs C6, D-25): a doc's "Anyone with the link"
// grant can also be published as a web page, at a readable address. It stays
// the same grant: its token and its address open the same page, its expiry
// and revoke end both, and the workspace's guest-access switch governs both,
// with publishing its one sub-option.
//
// ResolveExternal is the one access check for anyone outside the workspace,
// by token or by address. A doc's view link, and a published one, is served
// by the server's own script-free page (business/Publish), never the app: a
// grant ServedAsPage is refused by the collaboration service, the guest
// attachment route and the comment list, so its password can't be gone
// round.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// External is how someone outside the workspace names a grant: its link's
// token, or its published address.
type External struct {
	Token string
	Slug  string
}

var (
	// ErrGone is a published address whose link has expired: its page says
	// so (410), while an expired token stays the uniform "no longer
	// available".
	ErrGone = errors.New("guest: the link has expired")
	// ErrSlugTaken is an address another live link has.
	ErrSlugTaken = errors.New("guest: that address is taken")
	// ErrNotPublishable is a link that can't be a web page: not a doc's, or
	// not view-only (a published page is read-only).
	ErrNotPublishable = errors.New("guest: only a doc's view-only link can be published")
	// ErrPublishOff is a workspace that doesn't let pages be published.
	ErrPublishOff = errors.New("guest: publishing pages is off for this workspace")
)

// PublishInvalid is publishing settings that can't be used, saying why.
type PublishInvalid struct{ Reason string }

func (e *PublishInvalid) Error() string { return "guest: " + e.Reason }

// slugShape is an address: lower-case letters, numbers and dashes, 1 to 64,
// starting and ending with a letter or number (migration 350's CHECK).
var slugShape = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// Seams for tests: the grant reads and writes, and the doc.
var (
	grantByHash    = guestModel.GetActiveByHash
	grantBySlug    = guestModel.GetLiveBySlug
	grantByID      = guestModel.GetByID
	publishGrant   = guestModel.Publish
	unpublishGrant = guestModel.Unpublish
	docTitle       = func(ctx context.Context, docUUID string) string {
		if d, err := docBusiness.GetSystemDocByUUID(ctx, docUUID); err == nil && d != nil {
			return d.Title
		}
		return ""
	}
	docLive = grantedDocLive
	// The one switch and its one sub-option (business/Settings).
	guestOn        = settingsBusiness.GuestAccessEnabled
	publishOn      = settingsBusiness.GuestPublishEnabled
	publishAllowed = settingsBusiness.GuestPublishAllowed
)

// ServedAsPage is a grant the server's page serves, and nothing else may: a
// doc's view link, or one with a password.
func ServedAsPage(g *guestModel.GuestGrant) bool {
	return g != nil && g.ResourceType == guestModel.ResourceDoc && (g.Capability == guestModel.CapabilityView || g.PasswordHash != nil)
}

// ResolveExternal is the one access check for anyone outside the workspace:
// the grant a token or an address names, while guest access is on (and, for
// an address, its kind's sub-option), the grant is neither revoked nor expired, and its
// doc is live. Every refusal but an expired address is the same "not
// available" (ErrGuestDisabled, ErrInvalidGrant, ErrNotFound), so nothing can
// be learnt by probing; a read that failed is returned as it is.
func ResolveExternal(ctx context.Context, ref External) (*guestModel.GuestGrant, error) {
	if !guestOn() {
		return nil, ErrGuestDisabled
	}
	var g *guestModel.GuestGrant
	var err error
	switch {
	case ref.Slug != "":
		// An address answers by its kind's own sub-option (kinds.go): a
		// doc's page by publishing, a personal room by rooms. None asks the
		// database while every one is off.
		if !anyAddressOn() {
			return nil, ErrGuestDisabled
		}
		if !slugShape.MatchString(ref.Slug) {
			return nil, ErrInvalidGrant
		}
		if g, err = grantBySlug(ctx, ref.Slug); err != nil {
			return nil, err
		}
		if g != nil {
			if k := KindOf(g); !k.Addressed || k.AddressOn == nil || !k.AddressOn() {
				return nil, ErrGuestDisabled
			}
		}
		if g != nil && grantExpired(ctx, g) {
			return nil, ErrGone
		}
	case strings.TrimSpace(ref.Token) != "":
		if g, err = grantByHash(ctx, hashToken(strings.TrimSpace(ref.Token))); err != nil {
			return nil, err
		}
	default:
		return nil, ErrInvalidGrant
	}
	if g == nil || g.RevokedAt != nil || grantExpired(ctx, g) {
		return nil, ErrInvalidGrant
	}
	if err := docLive(ctx, g); err != nil {
		return nil, err
	}
	return g, nil
}

// PublishSettings are what a doc's owner sets on its view link.
type PublishSettings struct {
	// Slug is the page's address; empty keeps the one there, or makes one from
	// the doc's title.
	Slug string `json:"slug"`
	// Indexable is "Let search engines find this page"; off unless on.
	Indexable bool `json:"indexable"`
	// AllowCopies lets members copy the page into their own docs.
	AllowCopies bool `json:"allow_copies"`
	// Password: nil keeps the one there, "" takes it off, anything else sets it.
	Password *string `json:"password,omitempty"`
}

const (
	shortestPassword = 4
	longestPassword  = 128
)

// normaliseSlug is an address as typed, as a page keeps it: lower case, words
// joined by dashes, nothing else, at most 64. Pure.
func normaliseSlug(typed string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(typed)) {
		if b.Len() >= 64 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case unicode.IsSpace(r) || r == '-' || r == '_' || r == '.' || r == '/':
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// slugFor is a new page's address: its title's words and a few random
// letters, so two pages of one name don't meet and an address can't be
// guessed from a title alone.
func slugFor(title string) string {
	base := normaliseSlug(title)
	if len(base) > 48 {
		base = strings.Trim(base[:48], "-")
	}
	tail := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	if base == "" {
		return "page-" + tail
	}
	return base + "-" + tail
}

// PublishLink publishes a doc's view link as a web page, or changes how. The
// caller has checked that by may share the doc outside the workspace (its
// owner: the guest link rule); this checks the rest.
func PublishLink(ctx context.Context, by uuid.UUID, grantID uuid.UUID, in PublishSettings) (*guestModel.GuestGrant, error) {
	if !guestOn() {
		return nil, ErrGuestDisabled
	}
	if !publishAllowed() {
		return nil, ErrPublishOff
	}
	g, err := grantByID(ctx, grantID)
	if err != nil {
		return nil, err
	}
	if g == nil || g.RevokedAt != nil || grantExpired(ctx, g) {
		return nil, ErrInvalidGrant
	}
	if g.ResourceType != guestModel.ResourceDoc || g.Capability != guestModel.CapabilityView {
		return nil, ErrNotPublishable
	}
	if err := docLive(ctx, g); err != nil {
		return nil, err
	}

	slug := normaliseSlug(in.Slug)
	switch {
	case slug == "" && strings.TrimSpace(in.Slug) != "":
		return nil, &PublishInvalid{"An address is 1 to 64 letters, numbers and dashes, starting and ending with a letter or number."}
	case slug == "" && g.Slug != nil:
		slug = *g.Slug
	case slug == "":
		slug = slugFor(docTitle(ctx, g.ResourceID))
	}
	if !slugShape.MatchString(slug) {
		return nil, &PublishInvalid{"An address is 1 to 64 letters, numbers and dashes, starting and ending with a letter or number."}
	}

	p := guestModel.Publication{Slug: slug, Indexable: in.Indexable, AllowCopies: in.AllowCopies, PublishedBy: by,
		NewUnlock: g.Slug == nil || *g.Slug != slug}
	switch {
	case in.Password == nil:
		p.PasswordHash = g.PasswordHash
	case *in.Password == "":
		p.NewUnlock = p.NewUnlock || g.PasswordHash != nil
	default:
		n := utf8.RuneCountInString(*in.Password)
		if n < shortestPassword || n > longestPassword {
			return nil, &PublishInvalid{"A password is 4 to 128 characters."}
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(*in.Password), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		h := string(hash)
		p.PasswordHash, p.NewUnlock = &h, true
	}
	saved, err := publishGrant(ctx, g.Id, p)
	if errors.Is(err, guestModel.ErrSlugTaken) {
		return nil, ErrSlugTaken
	}
	if err != nil {
		return nil, err
	}
	if saved == nil {
		return nil, ErrInvalidGrant
	}
	return saved, nil
}

// UnpublishLink takes a link's page off the web, at once: its address stops
// answering, and its token keeps working until the link is revoked.
func UnpublishLink(ctx context.Context, grantID uuid.UUID) (*guestModel.GuestGrant, error) {
	g, err := unpublishGrant(ctx, grantID)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, ErrInvalidGrant
	}
	return g, nil
}

// PasswordOpens is whether password opens a grant's page.
func PasswordOpens(g *guestModel.GuestGrant, password string) bool {
	if g == nil || g.PasswordHash == nil || len(password) > 4*longestPassword {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(*g.PasswordHash), []byte(password)) == nil
}
