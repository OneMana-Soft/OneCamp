package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
)

// What an address answers, without the page: the same check as the page's
// own, saying nothing more than the page would.
func TestAnAddressSaysWhatItAnswers(t *testing.T) {
	prev := resolve
	t.Cleanup(func() { resolve = prev })
	pw, slug := "hash", "launch-notes"
	for _, c := range []struct {
		name  string
		grant *guestModel.GuestGrant
		err   error
		want  string
	}{
		{"a live page", &guestModel.GuestGrant{ResourceType: guestModel.ResourceDoc, Capability: guestModel.CapabilityView, Slug: &slug}, nil, StateLive},
		{"a password page", &guestModel.GuestGrant{ResourceType: guestModel.ResourceDoc, Capability: guestModel.CapabilityView, PasswordHash: &pw, Slug: &slug}, nil, StateLocked},
		{"an expired one", nil, guestBusiness.ErrGone, StateGone},
		{"never published", nil, guestBusiness.ErrInvalidGrant, StateMissing},
		{"publishing off", nil, guestBusiness.ErrGuestDisabled, StateMissing},
		{"a comment link", &guestModel.GuestGrant{ResourceType: guestModel.ResourceDoc, Capability: guestModel.CapabilityComment, Slug: &slug}, nil, StateMissing},
		{"another kind's address", &guestModel.GuestGrant{ResourceType: guestModel.ResourceMeeting, Slug: &slug}, nil, StateMissing},
	} {
		t.Run(c.name, func(t *testing.T) {
			resolve = func(context.Context, Ref) (*guestModel.GuestGrant, error) { return c.grant, c.err }
			got, err := PageState(context.Background(), Ref{Slug: "launch-notes"})
			if err != nil || got != c.want {
				t.Fatalf("%q %v, want %q", got, err, c.want)
			}
		})
	}
	resolve = func(context.Context, Ref) (*guestModel.GuestGrant, error) {
		return nil, errors.New("the database is away")
	}
	if _, err := PageState(context.Background(), Ref{Slug: "launch-notes"}); err == nil {
		t.Fatal("a failed read says so, never a state")
	}
}

// Every notice page is drawn in the page's own look: an ink drawing on the
// neutral tile, never an emoji, on a card, with the credit under it.
func TestNoticesAreInThePagesOwnLook(t *testing.T) {
	for name, d := range map[string]Document{
		"locked": LockedDocument("https://app.example.test/p/x?unlock=1"), "missing": MissingDocument(),
		"gone": GoneDocument(), "busy": BusyDocument(), "unavailable": UnavailableDocument(),
	} {
		body := string(d.Body)
		for _, r := range body {
			if r >= 0x2300 && r != 0x2019 && r != 0x201c && r != 0x201d && r != 0x00b7 {
				t.Errorf("%s: %q on the page", name, string(r))
				break
			}
		}
		for _, want := range []string{`<div class="ncard">`, `<div class="mark" aria-hidden="true"><svg`, `Made with OneCamp`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: no %s", name, want)
			}
		}
	}
}
