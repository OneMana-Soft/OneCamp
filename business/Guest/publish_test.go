package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// links is guest grants in memory, with the address rule the table keeps.
type links struct {
	byID      map[uuid.UUID]*guestModel.GuestGrant
	tokens    map[string]uuid.UUID // token hash (hex) → grant
	published []guestModel.Publication
}

func withLinks(t *testing.T) *links {
	t.Helper()
	l := &links{byID: map[uuid.UUID]*guestModel.GuestGrant{}, tokens: map[string]uuid.UUID{}}
	oldHash, oldSlug, oldID, oldPub, oldUnpub := grantByHash, grantBySlug, grantByID, publishGrant, unpublishGrant
	oldTitle, oldLive, oldOn, oldPubOn, oldAllowed := docTitle, grantLive, guestOn, publishOn, publishAllowed
	t.Cleanup(func() {
		grantByHash, grantBySlug, grantByID, publishGrant, unpublishGrant = oldHash, oldSlug, oldID, oldPub, oldUnpub
		docTitle, grantLive, guestOn, publishOn, publishAllowed = oldTitle, oldLive, oldOn, oldPubOn, oldAllowed
	})
	grantByHash = func(_ context.Context, hash []byte) (*guestModel.GuestGrant, error) {
		id, ok := l.tokens[string(hash)]
		if !ok {
			return nil, nil
		}
		g := l.byID[id]
		if g.RevokedAt != nil || grantExpired(context.Background(), g) {
			return nil, nil
		}
		return g, nil
	}
	grantBySlug = func(_ context.Context, slug string) (*guestModel.GuestGrant, error) {
		for _, g := range l.byID {
			if g.Slug != nil && *g.Slug == slug && g.RevokedAt == nil {
				return g, nil
			}
		}
		return nil, nil
	}
	grantByID = func(_ context.Context, id uuid.UUID) (*guestModel.GuestGrant, error) { return l.byID[id], nil }
	publishGrant = func(_ context.Context, id uuid.UUID, p guestModel.Publication) (*guestModel.GuestGrant, error) {
		g := l.byID[id]
		for _, o := range l.byID {
			if o.Id != id && o.Slug != nil && *o.Slug == p.Slug && o.RevokedAt == nil && o.ResourceID != g.ResourceID {
				return nil, guestModel.ErrSlugTaken
			}
		}
		for _, o := range l.byID {
			if o.Id != id && o.ResourceID == g.ResourceID {
				o.Slug = nil
			}
		}
		l.published = append(l.published, p)
		slug := p.Slug
		g.Slug, g.Indexable, g.AllowCopies, g.PasswordHash = &slug, p.Indexable, p.AllowCopies, p.PasswordHash
		if p.NewUnlock {
			g.UnlockVersion++
		}
		return g, nil
	}
	unpublishGrant = func(_ context.Context, id uuid.UUID) (*guestModel.GuestGrant, error) {
		g := l.byID[id]
		if g == nil || g.RevokedAt != nil {
			return nil, nil
		}
		g.Slug, g.PasswordHash, g.Indexable, g.AllowCopies = nil, nil, false, false
		g.UnlockVersion++
		return g, nil
	}
	docTitle = func(context.Context, string) string { return "Launch Notes" }
	grantLive = func(context.Context, *guestModel.GuestGrant) error { return nil }
	guestOn = func() bool { return true }
	publishOn = func() bool { return true }
	publishAllowed = func() bool { return true }
	return l
}

func (l *links) add(token, kind, capability, doc string) *guestModel.GuestGrant {
	g := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: kind, ResourceID: doc, Capability: capability, UnlockVersion: 1}
	l.byID[g.Id] = g
	l.tokens[string(hashToken(token))] = g.Id
	return g
}

func strp(s string) *string { return &s }

// One access check: a token and an address name the same grant, under the
// one switch (and, for an address, its sub-option); revoked, expired or a
// doc that's gone open nothing, an expired address says so.
func TestOneAccessCheckForTokenAndAddress(t *testing.T) {
	l := withLinks(t)
	ctx := context.Background()
	g := l.add("tok-view", "doc", "view", "d-1")
	g.Slug = strp("launch")
	for _, ref := range []External{{Token: "tok-view"}, {Slug: "launch"}} {
		got, err := ResolveExternal(ctx, ref)
		if err != nil || got.Id != g.Id {
			t.Errorf("%+v: %+v, %v", ref, got, err)
		}
	}
	for name, ref := range map[string]External{"no token": {}, "an unknown token": {Token: "nope"}, "a bad address": {Slug: "Not An Address"},
		"an unknown address": {Slug: "elsewhere"}} {
		if _, err := ResolveExternal(ctx, ref); !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("%s: %v", name, err)
		}
	}

	guestOn = func() bool { return false }
	for _, ref := range []External{{Token: "tok-view"}, {Slug: "launch"}} {
		if _, err := ResolveExternal(ctx, ref); !errors.Is(err, ErrGuestDisabled) {
			t.Errorf("guest access off, %+v: %v", ref, err)
		}
	}
	guestOn = func() bool { return true }
	publishOn = func() bool { return false }
	if _, err := ResolveExternal(ctx, External{Slug: "launch"}); !errors.Is(err, ErrGuestDisabled) {
		t.Errorf("publishing off, the address: %v", err)
	}
	if _, err := ResolveExternal(ctx, External{Token: "tok-view"}); err != nil {
		t.Errorf("publishing off, the token: %v", err)
	}
	publishOn = func() bool { return true }

	past := time.Now().Add(-time.Minute)
	g.ExpiresAt = &past
	if _, err := ResolveExternal(ctx, External{Slug: "launch"}); !errors.Is(err, ErrGone) {
		t.Errorf("an expired address: %v", err)
	}
	if _, err := ResolveExternal(ctx, External{Token: "tok-view"}); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("an expired token: %v", err)
	}
	g.ExpiresAt = nil
	now := time.Now()
	g.RevokedAt = &now
	if _, err := ResolveExternal(ctx, External{Slug: "launch"}); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("a revoked link's address: %v", err)
	}
	g.RevokedAt = nil
	grantLive = func(context.Context, *guestModel.GuestGrant) error { return ErrNotFound }
	if _, err := ResolveExternal(ctx, External{Token: "tok-view"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a doc that's gone: %v", err)
	}
}

// Only a doc's view link is the server's page alone; a comment link and a
// board's are the app's.
func TestWhatTheServersPageServes(t *testing.T) {
	hash := "x"
	for _, c := range []struct {
		g    guestModel.GuestGrant
		want bool
	}{
		{guestModel.GuestGrant{ResourceType: "doc", Capability: "view"}, true},
		{guestModel.GuestGrant{ResourceType: "doc", Capability: "comment"}, false},
		{guestModel.GuestGrant{ResourceType: "doc", Capability: "comment", PasswordHash: &hash}, true},
		{guestModel.GuestGrant{ResourceType: "board", Capability: "view"}, false},
	} {
		if got := ServedAsPage(&c.g); got != c.want {
			t.Errorf("%s/%s: %v", c.g.ResourceType, c.g.Capability, got)
		}
	}
	if ServedAsPage(nil) {
		t.Error("no grant is a page")
	}
}

// Publishing is a view link's option: an address (from the title when none is
// given), search engines, copies, a password; a new password or address ends
// every unlock; one published page a doc; the one switch and its sub-option.
func TestPublishingALink(t *testing.T) {
	l := withLinks(t)
	ctx := context.Background()
	by := uuid.New()
	view := l.add("tok-view", "doc", "view", "d-1")
	comment := l.add("tok-comment", "doc", "comment", "d-1")
	board := l.add("tok-board", "board", "view", "b-1")

	for name, id := range map[string]uuid.UUID{"a comment link": comment.Id, "a board's link": board.Id} {
		if _, err := PublishLink(ctx, by, id, PublishSettings{}); !errors.Is(err, ErrNotPublishable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	g, err := PublishLink(ctx, by, view.Id, PublishSettings{})
	if err != nil || g.Slug == nil || !strings.HasPrefix(*g.Slug, "launch-notes-") || len(*g.Slug) != len("launch-notes-")+8 {
		t.Fatalf("a first publish: %+v, %v", g, err)
	}
	if g.Indexable || g.AllowCopies || g.PasswordHash != nil || g.UnlockVersion != 2 {
		t.Errorf("a first publish is found by no search engine, uncopyable, open: %+v", g)
	}
	first := *g.Slug

	// A password set ends earlier unlocks; kept by a change that sends none.
	g, err = PublishLink(ctx, by, view.Id, PublishSettings{Indexable: true, Password: strp("open sesame")})
	if err != nil || !g.Indexable || g.PasswordHash == nil || *g.Slug != first || g.UnlockVersion != 3 || !PasswordOpens(g, "open sesame") {
		t.Fatalf("a password: %+v, %v", g, err)
	}
	g, _ = PublishLink(ctx, by, view.Id, PublishSettings{AllowCopies: true})
	if g.PasswordHash == nil || g.UnlockVersion != 3 || !g.AllowCopies || g.Indexable {
		t.Errorf("a change that sends no password keeps it: %+v", g)
	}
	g, _ = PublishLink(ctx, by, view.Id, PublishSettings{Slug: "Our Launch", Password: strp("")})
	if *g.Slug != "our-launch" || g.PasswordHash != nil || g.UnlockVersion != 4 {
		t.Errorf("a new address, the password off: %+v", g)
	}
	for name, in := range map[string]PublishSettings{
		"a bad address": {Slug: "!!!"}, "a short password": {Password: strp("abc")}, "a long password": {Password: strp(strings.Repeat("x", 129))},
	} {
		var invalid *PublishInvalid
		if _, err := PublishLink(ctx, by, view.Id, in); !errors.As(err, &invalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	other := l.add("tok-other", "doc", "view", "d-2")
	other.Slug = strp("taken")
	if _, err := PublishLink(ctx, by, view.Id, PublishSettings{Slug: "taken"}); !errors.Is(err, ErrSlugTaken) {
		t.Errorf("another doc's address: %v", err)
	}

	// One published page a doc: publishing a second view link moves it.
	second := l.add("tok-second", "doc", "view", "d-1")
	if g2, err := PublishLink(ctx, by, second.Id, PublishSettings{Slug: "second"}); err != nil || *g2.Slug != "second" || view.Slug != nil {
		t.Errorf("a second link of one doc: %+v, %v (first %v)", g2, err, view.Slug)
	}

	guestOn = func() bool { return false }
	if _, err := PublishLink(ctx, by, view.Id, PublishSettings{}); !errors.Is(err, ErrGuestDisabled) {
		t.Errorf("guest access off: %v", err)
	}
	guestOn = func() bool { return true }
	publishAllowed = func() bool { return false }
	if _, err := PublishLink(ctx, by, view.Id, PublishSettings{}); !errors.Is(err, ErrPublishOff) {
		t.Errorf("publishing off: %v", err)
	}
	publishAllowed = func() bool { return true }

	if g, err := UnpublishLink(ctx, second.Id); err != nil || g.Slug != nil {
		t.Errorf("unpublishing: %+v, %v", g, err)
	}
	now := time.Now()
	view.RevokedAt = &now
	if _, err := PublishLink(ctx, by, view.Id, PublishSettings{}); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("a revoked link: %v", err)
	}
	if _, err := UnpublishLink(ctx, view.Id); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("unpublishing a revoked link: %v", err)
	}
}

func TestAddressesAreTidy(t *testing.T) {
	for in, want := range map[string]string{
		"  Launch Notes 2026 ": "launch-notes-2026", "Q4/plan_v2.final": "q4-plan-v2-final", "--a--b--": "a-b",
		"Café ☕ menu": "caf-menu", "!!!": "", strings.Repeat("ab", 40): strings.Repeat("ab", 32),
	} {
		if got := normaliseSlug(in); got != want {
			t.Errorf("normaliseSlug(%q) = %q, want %q", in, got, want)
		}
	}
	if s := slugFor(""); !strings.HasPrefix(s, "page-") || !slugShape.MatchString(s) {
		t.Errorf("an untitled page's address %q", s)
	}
	if s := slugFor(strings.Repeat("word ", 30)); len(s) > 64 || !slugShape.MatchString(s) {
		t.Errorf("a long title's address %q", s)
	}
}
