package business

import (
	"context"
	"errors"
	"testing"
	"time"

	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
)

const day = 24 * time.Hour

// withCap sets the workspace's longest link lifetime for one test.
func withCap(t *testing.T, days int) {
	t.Helper()
	prev := linkCapDays
	t.Cleanup(func() { linkCapDays = prev })
	linkCapDays = func(context.Context) int { return days }
}

// A new link's lifetime: the ask, the default for none, the built-in ceiling
// without a cap and the cap as the ceiling with one; "until revoked" only
// without a cap, the cap itself under one.
func TestALinkLastsNoLongerThanTheWorkspaceAllows(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		cap   int
		ask   time.Duration
		never bool
		want  time.Duration // 0: until revoked
	}{
		{"no cap, nothing asked", 0, 0, false, resourceGrantTTL},
		{"no cap, a month", 0, 30 * day, false, 30 * day},
		{"no cap, past the ceiling", 0, 200 * day, false, maxResourceGrantTTL},
		{"no cap, never", 0, 0, true, 0},
		{"a 30-day cap, sixty asked", 30, 60 * day, false, 30 * day},
		{"a 30-day cap, never", 30, 0, true, 30 * day},
		{"a 30-day cap, a week", 30, 7 * day, false, 7 * day},
		{"a year's cap, past the built-in ceiling", 365, 200 * day, false, 200 * day},
		{"a 7-day cap, nothing asked", 7, 0, false, 7 * day},
	} {
		t.Run(c.name, func(t *testing.T) {
			withCap(t, c.cap)
			got := linkLifetime(ctx, KindLink, now, c.ask, c.never)
			switch {
			case c.want == 0 && got != nil:
				t.Fatalf("want until revoked, got %v", got)
			case c.want != 0 && (got == nil || !got.Equal(now.Add(c.want))):
				t.Fatalf("want %v, got %v", now.Add(c.want), got)
			}
		})
	}
}

// The cap at resolve: a link made the cap's days ago or more is past, unless
// it is standing (a personal room); no cap, nothing is past by age.
func TestTheCapStopsOlderLinksButNotStandingOnes(t *testing.T) {
	ctx := context.Background()
	slug := "dana"
	made := func(daysAgo int) time.Time { return time.Now().Add(-time.Duration(daysAgo) * day) }
	page := func(daysAgo int) *guestModel.GuestGrant {
		return &guestModel.GuestGrant{ResourceType: guestModel.ResourceDoc, Slug: &slug, CreatedAt: made(daysAgo)}
	}
	room := &guestModel.GuestGrant{ResourceType: guestModel.ResourceMeeting, Slug: &slug, CreatedAt: made(400)}

	withCap(t, 0)
	if grantExpired(ctx, page(400)) {
		t.Fatal("no cap: a link is never past by age")
	}
	withCap(t, 30)
	if !grantExpired(ctx, page(31)) || !grantExpired(ctx, page(30)) {
		t.Fatal("a 30-day cap: a link made 30 days ago or more is past")
	}
	if grantExpired(ctx, page(10)) {
		t.Fatal("a 30-day cap: a link made 10 days ago is live")
	}
	if grantExpired(ctx, room) {
		t.Fatal("a room is standing: the cap leaves it alone")
	}
	withCap(t, 365)
	if grantExpired(ctx, page(31)) {
		t.Fatal("raising the cap brings a link back: nothing was written")
	}
}

// The kind table: a doc with an address is a page, a meeting with one is a
// room (standing), anything else is a link by token.
func TestEveryGrantHasOneKind(t *testing.T) {
	slug := "x"
	for _, c := range []struct {
		g    *guestModel.GuestGrant
		want string
	}{
		{&guestModel.GuestGrant{ResourceType: guestModel.ResourceDoc, Slug: &slug}, "page"},
		{&guestModel.GuestGrant{ResourceType: guestModel.ResourceMeeting, Slug: &slug}, "room"},
		{&guestModel.GuestGrant{ResourceType: guestModel.ResourceDoc}, "link"},
		{&guestModel.GuestGrant{ResourceType: guestModel.ResourceMeeting}, "link"},
		{&guestModel.GuestGrant{ResourceType: guestModel.ResourceBoard, Slug: &slug}, "link"},
	} {
		if got := KindOf(c.g); got.Name != c.want {
			t.Errorf("%s with address %v: %s, want %s", c.g.ResourceType, c.g.Slug != nil, got.Name, c.want)
		}
		if KindOf(c.g).Standing != (c.want == "room") {
			t.Errorf("%s: standing %v", c.want, KindOf(c.g).Standing)
		}
	}
}

// An address answers by its own kind's sub-option: a room while pages are
// off, never while rooms are off, and nothing while guest access is off.
func TestAnAddressAnswersByItsKindsSwitch(t *testing.T) {
	ctx := context.Background()
	withCap(t, 0)
	slug := "dana-room"
	room := &guestModel.GuestGrant{ResourceType: guestModel.ResourceMeeting, Slug: &slug, Capability: guestModel.CapabilityJoin, CreatedAt: time.Now()}
	page := &guestModel.GuestGrant{ResourceType: guestModel.ResourceDoc, Slug: &slug, Capability: guestModel.CapabilityView, CreatedAt: time.Now()}
	prevSlug, prevGuest, prevPub, prevRooms, prevLive := grantBySlug, guestOn, publishOn, roomsOn, grantLive
	t.Cleanup(func() {
		grantBySlug, guestOn, publishOn, roomsOn, grantLive = prevSlug, prevGuest, prevPub, prevRooms, prevLive
	})
	grantLive = func(context.Context, *guestModel.GuestGrant) error { return nil }
	for _, c := range []struct {
		name                string
		g                   *guestModel.GuestGrant
		guest, pages, rooms bool
		want                error
	}{
		{"a room, rooms on, pages off", room, true, false, true, nil},
		{"a room, rooms off", room, true, true, false, ErrGuestDisabled},
		{"a page, pages off, rooms on", page, true, false, true, ErrGuestDisabled},
		{"a page, pages on", page, true, true, true, nil},
		{"a room, guest access off", room, false, true, true, ErrGuestDisabled},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := c.g
			grantBySlug = func(context.Context, string) (*guestModel.GuestGrant, error) { return g, nil }
			guestOn = func() bool { return c.guest }
			publishOn = func() bool { return c.guest && c.pages }
			roomsOn = func() bool { return c.guest && c.rooms }
			_, err := ResolveExternal(ctx, External{Slug: slug})
			if !errors.Is(err, c.want) && !(c.want == nil && err == nil) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}
