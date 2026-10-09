package business

import (
	"context"
	"errors"
	"testing"
)

// An address with the Kelvin sign or a dotted capital I is not matched to an
// account or admitted, whoever vouches for it: Unicode lowercasing turned
// "Kate@example.com" into kate@example.com, someone else's account. It is
// refused before any lookup (no store is reached in this test).
func TestAnAddressOutsideASCIIIsNeitherMatchedNorAdmitted(t *testing.T) {
	for _, addr := range []string{"Kate@example.com", "İnci@example.com"} {
		if _, err := AdmitOAuthUser(context.Background(), addr, "Someone", "google", "example.com"); !errors.Is(err, ErrAddressNotASCII) {
			t.Errorf("admitting %q: %v, want ErrAddressNotASCII", addr, err)
		}
		if _, err := JoinAsMember(context.Background(), addr, "Someone", nil, "google", false); !errors.Is(err, ErrAddressNotASCII) {
			t.Errorf("joining as %q: %v, want ErrAddressNotASCII", addr, err)
		}
		if onAllowList(addr, []string{"kate@example.com", "inci@example.com", "@example.com"}) {
			t.Errorf("%q matched the allow-list", addr)
		}
	}
	// GitHub never ranks such an address, so the ASCII one it has verified is
	// the one that signs in, whatever the other would have matched.
	asked := map[string]bool{}
	known := func(addr string) int {
		asked[addr] = true
		if addr == "kate@example.com" {
			return knownMember
		}
		return unknownAddress
	}
	got, ok := githubAddressToAdmit([]GitHubEmail{{"Kate@example.com", true, true}, {"kate.r@personal.example", false, true}}, known)
	if !ok || got != "Kate@example.com" || asked["Kate@example.com"] {
		t.Errorf("chose %q (asked %v): an address outside ASCII must never be ranked", got, asked)
	}
}
