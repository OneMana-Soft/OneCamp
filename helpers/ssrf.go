package helpers

import (
	"fmt"
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
// an address blockedPrefixes names. SSRFSafeClient checks again as it
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

// isLocalIP reports whether an IP is on the customer's own infrastructure:
// loopback, RFC1918 / IPv6-ULA private, or link-local. This is the inverse of
// the "is this a safe PUBLIC target" question — for local-only AI we want to
// permit ONLY these ranges.
func isLocalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
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

// blockedPrefixes are the addresses a request this server makes for someone
// else (a webhook, an import's download, an app's handler) must never reach:
// this host, the networks it sits on, the cloud's metadata service, and every
// range that isn't the public internet. isBlockedAddr adds what a table can't
// say: an IPv4 address written as IPv6, and one carried inside an IPv6
// address.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		// IPv4
		"0.0.0.0/8",       // "this network": a connection to 0.0.0.0 reaches this host
		"10.0.0.0/8",      // private
		"100.64.0.0/10",   // carrier-grade NAT, internal on many clouds (Alibaba's metadata is 100.100.100.200)
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local, where the cloud metadata service is (169.254.169.254)
		"172.16.0.0/12",   // private
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.88.99.0/24",  // 6to4 relays (deprecated)
		"192.168.0.0/16",  // private
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, and the broadcast address 255.255.255.255
		// IPv6
		"::/96",          // unspecified (::, which reaches this host), loopback (::1), IPv4-compatible (::127.0.0.1, deprecated)
		"64:ff9b:1::/48", // NAT64 for local use
		"100::/64",       // discard
		"2001:db8::/32",  // documentation
		"fc00::/7",       // unique local: the private ranges of IPv6
		"fe80::/10",      // link-local
		"fec0::/10",      // site-local (deprecated)
		"ff00::/8",       // multicast
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// nat64 and sixToFour are the IPv6 ranges whose packets are sent on to the
// IPv4 address inside them: by a NAT64 gateway (RFC 6052) or a 6to4 tunnel
// (RFC 3056). On a host with a NAT64 route, 64:ff9b::10.0.0.1 is 10.0.0.1, so
// it's the address inside that gets checked.
var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
)

// isBlockedAddr reports whether a is an address this server must not connect
// to on someone else's behalf (see blockedPrefixes). Something that isn't an
// address is blocked.
func isBlockedAddr(a netip.Addr) bool {
	if !a.IsValid() {
		return true
	}
	// No prefix contains an address with a zone (fe80::1%eth0), nor an IPv4
	// address written as IPv6 (::ffff:127.0.0.1): both come off first.
	a = a.WithZone("").Unmap()
	if inner, ok := carriedIPv4(a); ok && isBlockedAddr(inner) {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// carriedIPv4 is the IPv4 address a NAT64 or 6to4 address sends its packets
// on to.
func carriedIPv4(a netip.Addr) (netip.Addr, bool) {
	b := a.As16()
	switch {
	case nat64.Contains(a):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFour.Contains(a):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
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
// else chose. Every connection it makes is checked against blockedPrefixes as
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
