//go:build integration

package provider

import (
	"net/netip"

	"github.com/akashc777/OneCamp/helpers/safefetch"
)

// AllowLocalSitesForTest lets a site on this machine (an integration test's
// fake, which listens on loopback) be connected to, and keeps every other
// private address refused; restore puts the rules in force back. It exists
// only in the integration build: in a server, IMPORT_PRIVATE_HOSTS is the only
// way to widen the rules, and it never admits loopback.
func AllowLocalSitesForTest() (restore func()) {
	old := sitePolicy
	useSitePolicy(safefetch.Policy{AnyPort: true, PrivateNetwork: true, Also: func(_ string, a netip.Addr) error {
		if a = a.Unmap(); a.IsLoopback() || safefetch.PublicAddr(a) {
			return nil
		}
		return safefetch.ErrBlocked
	}})
	return func() { useSitePolicy(old) }
}
