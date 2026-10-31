package business

import (
	"errors"
	"testing"
	"time"

	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
)

// What an address answers, by kind: a page's address answers for a doc's
// view link only; another kind's grant at the same address is missing; an
// expired one is gone, a refused one missing, and a failed read says so,
// never a state.
func TestAnAddressAnswersForItsOwnKind(t *testing.T) {
	slug, pw := "launch-notes", "hash"
	page := &guestModel.GuestGrant{ResourceType: guestModel.ResourceDoc, Capability: guestModel.CapabilityView, Slug: &slug}
	locked := &guestModel.GuestGrant{ResourceType: guestModel.ResourceDoc, Capability: guestModel.CapabilityView, Slug: &slug, PasswordHash: &pw}
	comment := &guestModel.GuestGrant{ResourceType: guestModel.ResourceDoc, Capability: guestModel.CapabilityComment, Slug: &slug}
	room := &guestModel.GuestGrant{ResourceType: guestModel.ResourceMeeting, Slug: &slug}
	lockedRoom := &guestModel.GuestGrant{ResourceType: guestModel.ResourceMeeting, Slug: &slug, PasswordHash: &pw}
	for _, c := range []struct {
		name string
		kind Kind
		g    *guestModel.GuestGrant
		err  error
		want string
	}{
		{"a live page", KindPage, page, nil, StateLive},
		{"a password page", KindPage, locked, nil, StateLocked},
		{"a comment link's address", KindPage, comment, nil, StateMissing},
		{"a room at a page's address", KindPage, room, nil, StateMissing},
		{"a page at a room's address", KindRoom, page, nil, StateMissing},
		{"a live room", KindRoom, room, nil, StateLive},
		{"a room with a passcode", KindRoom, lockedRoom, nil, StateLocked},
		{"expired", KindPage, nil, ErrGone, StateGone},
		{"never published", KindPage, nil, ErrInvalidGrant, StateMissing},
		{"off", KindRoom, nil, ErrGuestDisabled, StateMissing},
		{"its doc deleted", KindPage, nil, ErrNotFound, StateMissing},
	} {
		got, err := StateFor(c.kind, c.g, c.err)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, err, c.want)
		}
	}
	if got, err := StateFor(KindPage, nil, errors.New("the database is away")); err == nil || got != "" {
		t.Fatalf("a failed read said %q", got)
	}
}

// Each kind's address and its asks' surfaces are its own.
func TestEachKindHasItsOwnAddressAndSurfaces(t *testing.T) {
	if KindPage.Address("launch-notes") != "/public/p/launch-notes" || KindPage.WebAddress("launch-notes") != "/p/launch-notes" {
		t.Fatalf("a page answers at %q and %q", KindPage.Address("launch-notes"), KindPage.WebAddress("launch-notes"))
	}
	// A room's, as router.go mounts it and the web app's /r/[slug] page.
	if KindRoom.Address("maya-chen") != "/public/r/maya-chen" || KindRoom.WebAddress("maya-chen") != "/r/maya-chen" {
		t.Fatalf("a room answers at %q and %q", KindRoom.Address("maya-chen"), KindRoom.WebAddress("maya-chen"))
	}
	if KindPage.Surface("") != "page" || KindPage.Surface("file") != "page-file" || KindRoom.Surface("") == KindPage.Surface("") ||
		KindRoom.Surface("unlock") != "room-unlock" {
		t.Fatal("surfaces are shared between kinds")
	}
	// A room's passcode opens it for a working day, signed apart from a page's.
	if KindRoom.UnlockFor != 12*time.Hour || KindRoom.unlockLabel == KindPage.unlockLabel {
		t.Fatalf("a room's unlock: %v, %q", KindRoom.UnlockFor, KindRoom.unlockLabel)
	}
}
