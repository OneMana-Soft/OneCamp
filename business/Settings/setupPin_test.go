package business

import "testing"

// The first-admin endpoint is open to the world until it has been used once. On a
// self-hosted install that window runs from `make install` finishing until the
// operator has deployed a frontend and opened it, which is hours, not seconds.
// These pin the rule that closes it.

// TestSetupIsOpenWhenNothingWasPinned is the behaviour every existing install had,
// and the demo still has: no ADMIN_EMAIL_ID, anyone may claim the workspace.
func TestSetupIsOpenWhenNothingWasPinned(t *testing.T) {
	if !SetupPermittedFor("", "anyone@example.com") {
		t.Fatal("an unpinned install must still accept its first admin")
	}
}

// TestSetupRefusesAnyoneButThePinnedAddress is the point of the pin.
func TestSetupRefusesAnyoneButThePinnedAddress(t *testing.T) {
	if SetupPermittedFor("owner@example.com", "attacker@example.com") {
		t.Fatal("a pinned install accepted a stranger as its first admin")
	}
	if !SetupPermittedFor("owner@example.com", "owner@example.com") {
		t.Fatal("a pinned install refused the address it was pinned to")
	}
}

// TestSetupPinIgnoresCaseAndWhitespace guards the lock-out. The Makefile writes
// whatever the operator typed, the setup form sends whatever they typed there, and
// a capital letter or a trailing space in either place must not leave the real
// operator on the wrong side of their own server.
func TestSetupPinIgnoresCaseAndWhitespace(t *testing.T) {
	for _, tc := range []struct{ pin, email string }{
		{"Owner@Example.com", "owner@example.com"},
		{"owner@example.com", "  OWNER@example.com "},
		{" owner@example.com\n", "owner@example.com"},
	} {
		if !SetupPermittedFor(tc.pin, tc.email) {
			t.Errorf("pin %q should permit %q", tc.pin, tc.email)
		}
	}
}

// TestSetupPlaceholderIsNotAPin guards the lock-out that the literal reading has.
// The template ships ADMIN_EMAIL_ID as a __CHANGE_ME__ marker, and an install that
// never ran update-admin-email would otherwise be pinned to an address nobody can
// type, and claimable by nobody, forever.
func TestSetupPlaceholderIsNotAPin(t *testing.T) {
	for _, raw := range []string{
		"__CHANGE_ME_RUN_make_update-admin-email__",
		"",
		"   ",
		"not-an-address",
	} {
		if got := installAdminEmailFrom(raw); got != "" {
			t.Errorf("%q read as a pin (%q); it must read as no pin", raw, got)
		}
	}
	if got := installAdminEmailFrom(" Owner@Example.com "); got != "owner@example.com" {
		t.Errorf("a real address should be kept, normalised; got %q", got)
	}
}

// The pinned owner's address is compared after lowercasing A to Z only: the
// Kelvin sign lowercases to "k" the Unicode way, which let "\u212Aate@..."
// claim a workspace pinned to kate@....
func TestThePinIsNotClaimedWithALookalikeAddress(t *testing.T) {
	if SetupPermittedFor("kate@example.com", "\u212Aate@example.com") {
		t.Error("an address with the Kelvin sign claimed the workspace pinned to kate@example.com")
	}
	if !SetupPermittedFor("Kate@Example.com", " kate@example.COM ") {
		t.Error("the pinned owner, in another case, is refused")
	}
}
