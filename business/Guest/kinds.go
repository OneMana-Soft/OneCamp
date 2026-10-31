package business

// The kinds of link, in one table (docs C6-2, agreed with CALLS-LEAD): for
// each, whether it may have an address, the sub-option of guest access that
// lets its address answer, and whether it is standing, which the workspace's
// longest link lifetime (guest.link.max_days) leaves alone. A doc's page and a
// personal meeting room (calls CALLS-2) both answer at an address; a room is
// standing, a page is not.
//
// Each row also says where its address answers (on this server and in the
// web app), how long a password's unlock opens it (unlock.go), and which of
// its grants it serves; what an address answers now is addresses.go's. One
// namespace of slugs serves every kind, so two kinds never share an address.

import (
	"context"
	"time"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
)

// Kind is one row of the table.
type Kind struct {
	Name string
	// Addressed: the kind may be published at an address.
	Addressed bool
	// AddressOn is the sub-option its address answers under, nil for none.
	AddressOn func() bool
	// Standing: the cap leaves it alone.
	Standing bool
	// Path and WebPath are where its address answers on this server and in
	// the web app, before the slug ("/public/p/" and "/p/" for a page). The
	// router mounts them as literals, which router/publicSurface_test.go
	// reads, and addresses_test.go holds the two to the same strings.
	Path, WebPath string
	// UnlockFor is how long a password's unlock opens it; a kind without one
	// gives no unlocks.
	UnlockFor time.Duration
	// Opens is whether a grant of the kind is one its address serves (a page
	// is a doc's view link); nil serves every grant of the kind.
	Opens func(*guestModel.GuestGrant) bool
	// unlockLabel keeps one kind's unlocks from opening another's (unlock.go);
	// "" is "onecamp/guest/unlock/<Name>".
	unlockLabel string

	// What publishing a grant at this kind's address takes (PublishLink,
	// agreed with CALLS-LEAD, 20 Oct): which grants may be published as it
	// (publishable; a kind without it never is), whether the admin lets
	// owners publish it now (allowed: its sub-option alone, as guest access
	// is asked first) and the error that says not, the words a new address
	// starts from (slugBase; never empty), whether a new address tries those
	// words alone before adding letters (bareFirst), and whether search
	// engines and copies mean anything for it (pageOptions).
	publishable func(*guestModel.GuestGrant) bool
	allowed     func() bool
	offErr      error
	slugBase    func(context.Context, *guestModel.GuestGrant) string
	bareFirst   bool
	pageOptions bool
}

// publishUnlockLabel is the label a doc's page has signed its unlocks with
// since D-25: kept, so no unlock given before the table had it stops working.
const publishUnlockLabel = "onecamp/publish/unlock"

// pageUnlockFor is how long a password opens a doc's page, and
// roomUnlockFor how long a passcode opens a personal room's address: a
// working day, so a guest who leaves and comes back isn't asked again.
const (
	pageUnlockFor = 12 * time.Hour
	roomUnlockFor = 12 * time.Hour
)

// The kinds with an address. Every other grant is a link by token only,
// capped, with no sub-option of its own; a doc's view link by token is
// served as the same page as an address is, and unlocks as one.
var (
	KindPage = Kind{Name: "page", Addressed: true, AddressOn: func() bool { return publishOn() },
		Path: "/public/p/", WebPath: "/p/", UnlockFor: pageUnlockFor, Opens: docViewLink, unlockLabel: publishUnlockLabel,
		publishable: docViewLink, allowed: func() bool { return publishAllowed() }, offErr: ErrPublishOff,
		slugBase: func(ctx context.Context, g *guestModel.GuestGrant) string { return docTitle(ctx, g.ResourceID) }, pageOptions: true}
	KindRoom = Kind{Name: "room", Addressed: true, AddressOn: func() bool { return roomsOn() }, Standing: true,
		Path: "/public/r/", WebPath: "/r/", UnlockFor: roomUnlockFor, Opens: meetingLink,
		publishable: roomLink, allowed: func() bool { return roomsAllowed() }, offErr: ErrRoomsOff,
		slugBase: roomSlugBase, bareFirst: true}
	KindLink = Kind{Name: "link", UnlockFor: pageUnlockFor, unlockLabel: publishUnlockLabel}
)

// addressed is every kind with an address, in the order a grant is matched
// against them (addressKind): a further kind is a row here and nothing else.
var addressed = []Kind{KindPage, KindRoom}

// addressKind is the row a grant would take at an address: the first
// addressed kind whose publishable takes it. KindOf stays the row of a grant
// as it is. Pure but for the table.
func addressKind(g *guestModel.GuestGrant) (Kind, bool) {
	for _, k := range addressed {
		if k.publishable != nil && k.publishable(g) {
			return k, true
		}
	}
	return Kind{}, false
}

// roomLink is a personal room's link (calls CALLS-2): a meeting's join link
// that never ends. A meeting's link for one call has an end, and is never a room.
func roomLink(g *guestModel.GuestGrant) bool {
	return g != nil && g.ResourceType == guestModel.ResourceMeeting && g.Capability == guestModel.CapabilityJoin && g.ExpiresAt == nil
}

// meetingLink is a meeting's guest link: what a room's address serves.
func meetingLink(g *guestModel.GuestGrant) bool {
	return g != nil && g.ResourceType == guestModel.ResourceMeeting
}

// roomSlugBase is a new room's words: its owner's name, "room" for none.
func roomSlugBase(ctx context.Context, g *guestModel.GuestGrant) string {
	if name := ownerName(ctx, g.CreatedBy); normaliseSlug(name) != "" {
		return name
	}
	return "room"
}

// docViewLink is a doc's view link: what a page serves (a comment link opens
// the app, never a page).
func docViewLink(g *guestModel.GuestGrant) bool {
	return g != nil && g.ResourceType == guestModel.ResourceDoc && g.Capability == guestModel.CapabilityView
}

// Address is where the kind's address slug answers on this server, from its
// root: "/public/p/<slug>" for a page.
func (k Kind) Address(slug string) string { return k.Path + slug }

// WebAddress is where it answers in the web app, from its root: "/p/<slug>".
func (k Kind) WebAddress(slug string) string { return k.WebPath + slug }

// Serves reports whether g is a grant of this kind that its address serves.
func (k Kind) Serves(g *guestModel.GuestGrant) bool {
	return g != nil && KindOf(g).Name == k.Name && (k.Opens == nil || k.Opens(g))
}

// Surface names what is asked of the kind's address, for its per-address
// limit (redisStore.AllowPerAddress):
// the kind's name for the address itself, "<name>-<part>" for a part of it
// (a page's "page-file", "page-unlock"), so each kind counts apart.
func (k Kind) Surface(part string) string {
	if part == "" {
		return k.Name
	}
	return k.Name + "-" + part
}

// roomsOn is whether a room's address answers (guest access and the rooms
// sub-option), and roomsAllowed the sub-option alone, as the admin set it:
// seams for tests (business/Settings).
var (
	roomsOn      = settingsBusiness.GuestRoomsEnabled
	roomsAllowed = settingsBusiness.GuestRoomsAllowed
)

// ownerName is the name of the member who made a grant as people outside
// the workspace may see it (a room's public address is made from it), never
// their address, "" when there is none to read: a seam for tests.
var ownerName = userDomain.NameForGuests

// KindOf is the row a grant belongs to. Pure but for the table's switches.
func KindOf(g *guestModel.GuestGrant) Kind {
	switch {
	case guestModel.IsStanding(g):
		return KindRoom
	case g != nil && g.ResourceType == guestModel.ResourceDoc && g.Slug != nil:
		return KindPage
	}
	return KindLink
}

// anyAddressOn reports whether any kind's address may answer now: none does
// while every addressed kind's sub-option is off.
func anyAddressOn() bool {
	return publishOn() || roomsOn()
}

// linkCapDays is the workspace's longest link lifetime, a seam for tests.
var linkCapDays = func(ctx context.Context) int {
	return policyBusiness.Policies.Int(ctx, policyBusiness.GuestLinkMaxDays)
}

func init() {
	// Every live-grant query in the model asks the same cap.
	guestModel.LinkCapDays = func(ctx context.Context) int { return linkCapDays(ctx) }
}

// LinkCapDays is the workspace's longest link lifetime in days, 0 for none:
// what the share dialog offers up to.
func LinkCapDays(ctx context.Context) int { return linkCapDays(ctx) }

// capDays is the cap as a duration, 0 for none.
func capDays(ctx context.Context) time.Duration {
	if d := linkCapDays(ctx); d > 0 {
		return time.Duration(d) * 24 * time.Hour
	}
	return 0
}

// linkLifetime is how long a new link of kind k lasts: a standing kind's (a
// personal room) until revoked, as the cap leaves it alone (agreed with
// DOCS-LEAD-2, 20 Oct: one rule, no branch for rooms); any other, what was
// asked (the default for nothing), never past the ceiling, which is the
// workspace's cap when it has one and the built-in 90 days when not; and
// "until revoked" only while there is no cap, the cap itself under one. nil
// expiry means until revoked.
func linkLifetime(ctx context.Context, k Kind, now time.Time, ask time.Duration, never bool) *time.Time {
	if k.Standing {
		return nil
	}
	capped := capDays(ctx)
	if never && capped == 0 {
		return nil
	}
	switch {
	case never:
		// Asked for a link that never ends, under a cap: the longest there is.
		ask = capped
	case ask <= 0:
		ask = resourceGrantTTL
	}
	// The workspace's cap, when an admin set one, is the ceiling; without
	// one, the built-in ceiling is.
	ceiling := maxResourceGrantTTL
	if capped > 0 {
		ceiling = capped
	}
	if ask > ceiling {
		ask = ceiling
	}
	at := now.Add(ask)
	return &at
}

// LinkEnds is when a link stops opening what it shares: its own end, or the
// cap's days after it was made when that comes first (a standing link is never
// capped); nil for never. The SQL side is the model's liveSQL.
func LinkEnds(ctx context.Context, g *guestModel.GuestGrant) *time.Time {
	end := g.ExpiresAt
	if capped := capDays(ctx); capped > 0 && !guestModel.IsStanding(g) {
		if c := g.CreatedAt.Add(capped); end == nil || c.Before(*end) {
			end = &c
		}
	}
	return end
}
