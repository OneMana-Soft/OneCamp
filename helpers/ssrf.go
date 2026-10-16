package helpers

import (
	"errors"
	"fmt"
	"github.com/akashc777/OneCamp/helpers/safefetch"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ValidateOutboundURL checks that a URL is safe for server-side HTTP requests.
// It enforces HTTPS (outside dev) and refuses a host that is, or resolves to,
// an address isBlockedAddr refuses. SSRFSafeClient checks again as it
// connects, which is the check that counts: a name can resolve differently
// a moment later.
func ValidateOutboundURL(rawURL string, allowHTTPInDev bool) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "https" {
		if u.Scheme == "http" && allowHTTPInDev {
			// allowed in dev only
		} else {
			return nil, fmt.Errorf("URL must use HTTPS scheme")
		}
	}

	if u.Host == "" {
		return nil, fmt.Errorf("URL missing host")
	}

	host := u.Hostname()

	// Reject plain IP literals that are unsafe (netip reads a zone, which
	// net.ParseIP refuses: [fe80::1%25eth0] is an address, not a name).
	if a, err := netip.ParseAddr(host); err == nil {
		if isBlockedAddr(a) {
			return nil, fmt.Errorf("blocked IP address: %s", host)
		}
	} else {
		// For hostnames, resolve and check all returned IPs
		ips, err := net.LookupIP(host)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve host: %w", err)
		}
		for _, ip := range ips {
			if isBlockedIP(ip) {
				return nil, fmt.Errorf("blocked resolved IP for host %s: %s", host, ip)
			}
		}
	}

	return u, nil
}

// isLocalIP reports whether an IP is on the customer's own infrastructure, in
// the one sense this server uses (safefetch.PrivateNetworkAddr): loopback, the
// private ranges and the shared address space Tailscale uses, never a cloud's
// metadata or link-local address. For local-only AI, which permits ONLY these.
func isLocalIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	return ok && safefetch.PrivateNetworkAddr(a.Unmap())
}

// IsLocalNetworkHost reports whether a host (IP literal or DNS name) resolves
// exclusively to on-infrastructure addresses (loopback / private / link-local).
// Used by local-only AI mode to guarantee no content can leave to a cloud
// endpoint. Fails CLOSED: an empty host, a resolution error, or ANY public IP
// yields (false, err|nil) so the caller refuses egress.
func IsLocalNetworkHost(host string) (bool, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return false, fmt.Errorf("empty host")
	}
	if ip := net.ParseIP(host); ip != nil {
		return isLocalIP(ip), nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return false, fmt.Errorf("resolve host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return false, fmt.Errorf("host %q resolved to no addresses", host)
	}
	for _, ip := range ips {
		if !isLocalIP(ip) {
			return false, nil // any public IP → not local
		}
	}
	return true, nil
}

// isBlockedAddr reports whether a is an address this server must not connect
// to on someone else's behalf (a webhook, an import's download, an app's
// handler): this host, the networks it sits on, the cloud's metadata
// service, every range that isn't the public internet, and an IPv4 address
// written as IPv6 or carried inside one. It is safefetch's rule, the one
// table both use (an unfurl's fetch and this client), so a fix to one is a
// fix to both: the stricter of the two tables they had, which also refuses
// 2001::/23 (Teredo), all of 6to4, 3fff::/20 and any IPv6 address outside
// 2000::/3. Something that isn't an address is blocked.
func isBlockedAddr(a netip.Addr) bool {
	return !safefetch.PublicAddr(a)
}

// isBlockedIP is isBlockedAddr for a net.IP, the form the resolver returns.
func isBlockedIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	return !ok || isBlockedAddr(a)
}

// refuseBlockedAddr is the safe client's dial check (a net.Dialer's Control).
// It runs as each connection is made, with the address being connected to,
// after the name was resolved. So a name that resolved to a public address
// when its URL was checked and to this host when it's dialled (DNS rebinding)
// is refused, and so is any other address the dialler falls back to.
func refuseBlockedAddr(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("SSRF dial refused %q: not an IP address", address)
	}
	if isBlockedAddr(a) {
		return fmt.Errorf("SSRF dial blocked IP %s", a)
	}
	return nil
}

// SSRFSafeClient returns an *http.Client for requests to an address someone
// else chose. Every connection it makes is checked with isBlockedAddr as
// it's made (refuseBlockedAddr), and every redirect's URL as it's followed.
//
// It has no proxy, and mustn't: through a proxy, the address dialled would be
// the proxy's, and the check would vet the wrong host.
func SSRFSafeClient(allowHTTPInDev bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: refuseBlockedAddr}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			_, err := ValidateOutboundURL(req.URL.String(), allowHTTPInDev)
			if err != nil {
				return fmt.Errorf("redirect blocked: %w", err)
			}
			return nil
		},
	}
}

// EndpointRefusal words why an endpoint was refused by safefetch's
// EndpointPolicy, for the person who typed it, or "" when err is not such a
// refusal. addedByAdmin is the policy it was checked under.
func EndpointRefusal(err error, addedByAdmin bool) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, safefetch.ErrScheme):
		return "Use an address that starts with https:// or http://."
	case errors.Is(err, safefetch.ErrUserInfo):
		return "Take the user name and password out of the address, and put the secret in its own field."
	case errors.Is(err, safefetch.ErrPort):
		return "That port can't be used."
	case errors.Is(err, safefetch.ErrOffOrigin):
		return "The server sent us to another address, so nothing was sent there. Enter the address it moved to."
	case errors.Is(err, safefetch.ErrHost), errors.Is(err, safefetch.ErrBlocked):
		if addedByAdmin {
			return "That address can't be reached from here: it is a cloud metadata, link-local or other special address."
		}
		return "That address is on this server's own network. Only an admin can add a server or agent there."
	}
	return ""
}
