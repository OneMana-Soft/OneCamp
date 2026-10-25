package provider

// The outbound rules for a provider whose host the admin types (a Jira site).
//
// Its requests carry the admin's credential to that host, so they go through
// helpers/safefetch: public addresses on any port (a self-hosted site's port
// is part of its address), same-origin redirects only. A server whose operator
// runs such a site on its own network lists it in IMPORT_PRIVATE_HOSTS, read
// once at start-up: exact host names, addresses or CIDR ranges, comma
// separated. Loopback, link-local and the clouds' metadata services stay
// refused even when listed. It is an install setting: nothing a workspace
// admin can change reaches it.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers/safefetch"
)

// privateHostsEnv names the setting.
const privateHostsEnv = "IMPORT_PRIVATE_HOSTS"

// privateHosts are the places on this server's own network an operator
// listed: ranges, and names with the addresses they resolved to at start-up.
type privateHosts struct {
	nets  []netip.Prefix
	names map[string][]netip.Addr
}

// parsePrivateHosts reads the setting's list into ranges (an address is a
// range of one) and host names. An entry that is none of these is an error
// naming it: a list is applied whole or not at all. Pure.
func parsePrivateHosts(list string) (nets []netip.Prefix, names []string, err error) {
	for _, raw := range strings.Split(list, ",") {
		e := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			p, perr := netip.ParsePrefix(e)
			if perr != nil {
				return nil, nil, fmt.Errorf("%s: %q isn't a CIDR range (like 10.20.0.0/16)", privateHostsEnv, raw)
			}
			nets = append(nets, p.Masked())
			continue
		}
		if a, aerr := netip.ParseAddr(e); aerr == nil {
			nets = append(nets, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
			continue
		}
		if _, cerr := safefetch.CheckEndpoint("https://"+e, safefetch.Policy{PrivateNetwork: true}); cerr != nil || strings.ContainsAny(e, "/:@?#") {
			return nil, nil, fmt.Errorf("%s: %q isn't a host name, an address or a CIDR range", privateHostsEnv, raw)
		}
		names = append(names, e)
	}
	return nets, names, nil
}

// allows is the listed hosts as safefetch's Policy.Also, for every address a
// site connection is about to use: a public one, or a private one inside a
// listed range or among a listed name's addresses when that name is the host.
// Loopback never: PrivateNetwork would let it through, so it is refused here.
func (h privateHosts) allows(host string, a netip.Addr) error {
	a = a.Unmap()
	if safefetch.PublicAddr(a) {
		return nil
	}
	if a.IsLoopback() {
		return safefetch.ErrBlocked
	}
	for _, n := range h.nets {
		if n.Contains(a) {
			return nil
		}
	}
	if slices.Contains(h.names[strings.ToLower(strings.TrimSuffix(host, "."))], a) {
		return nil
	}
	return safefetch.ErrBlocked
}

// policy is the rule for a site: public only, or, with hosts listed, this
// server's own network as far as allows lets it.
func (h privateHosts) policy() safefetch.Policy {
	if len(h.nets) == 0 && len(h.names) == 0 {
		return safefetch.Policy{AnyPort: true}
	}
	return safefetch.Policy{AnyPort: true, PrivateNetwork: true, Also: h.allows}
}

var (
	// sitePolicy is the rule in force; public only until LoadPrivateHosts.
	sitePolicy = privateHosts{}.policy()
	// SiteAPIClient is SharedAPIClient for a host the admin typed: it
	// connects only where sitePolicy allows, checked as each connection is
	// made, and follows a redirect only on the origin it was sent to.
	SiteAPIClient = newSiteClient(sitePolicy)
)

// newSiteClient is safefetch's client under p, with SharedAPIClient's wait
// for an answer's headers.
func newSiteClient(p safefetch.Policy) *http.Client {
	c := safefetch.NewClient(p, 60*time.Second)
	c.Transport.(*http.Transport).ResponseHeaderTimeout = 30 * time.Second
	return c
}

// useSitePolicy puts p in force for CheckSite and SiteAPIClient alike. The
// client is changed in place: providers hold it from their start.
func useSitePolicy(p safefetch.Policy) {
	sitePolicy = p
	*SiteAPIClient = *newSiteClient(p)
}

// LoadPrivateHosts reads IMPORT_PRIVATE_HOSTS, once, as the server starts. An
// entry it can't read is an error, and the server doesn't start with half a
// list. A listed name is looked up now, and only the addresses it has now are
// let through; one that doesn't resolve is answered as a warning, in words,
// and lets nothing through until the server starts again.
func LoadPrivateHosts(ctx context.Context) (warnings []string, err error) {
	nets, names, err := parsePrivateHosts(os.Getenv(privateHostsEnv))
	if err != nil {
		return nil, err
	}
	h := privateHosts{nets: nets, names: map[string][]netip.Addr{}}
	for _, n := range names {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addrs, lerr := net.DefaultResolver.LookupNetIP(lctx, "ip", n)
		cancel()
		if lerr != nil || len(addrs) == 0 {
			warnings = append(warnings, fmt.Sprintf("%s lists %s, which doesn't resolve now; imports can't reach it until the server starts again", privateHostsEnv, n))
			continue
		}
		for _, a := range addrs {
			h.names[n] = append(h.names[n], a.Unmap())
		}
	}
	useSitePolicy(h.policy())
	return warnings, nil
}

// CheckSite applies the site rules that need no lookup to an address the
// admin typed, so a connection to it is refused before it is tried or saved.
// An address written as one is held to the listed hosts as the dialler holds
// it.
func CheckSite(raw string) error {
	u, err := safefetch.CheckEndpoint(raw, sitePolicy)
	if err != nil {
		return err
	}
	if a, perr := netip.ParseAddr(u.Hostname()); perr == nil && sitePolicy.Also != nil {
		return sitePolicy.Also(u.Hostname(), a)
	}
	return nil
}
