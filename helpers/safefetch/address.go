// Package safefetch fetches an address someone gave the server, such as a link
// pasted into a doc, without letting them aim the server at itself or at the
// network it sits in (server-side request forgery).
//
// WHY IT EXISTS. A link preview is the server opening a URL the person chose.
// Fetched naively, http://169.254.169.254/ reads the cloud's instance
// credentials, http://127.0.0.1:6379/ talks to Redis, and http://10.0.0.5/
// reaches whatever sits beside the box on a customer's network. OneCamp is
// self-hosted, so "the network" is the customer's own.
//
// HOW. Four rules, each enforced where it can't be skipped:
//   - Only http and https, only the usual web ports, no user:password@ in the
//     link, and no names that only mean something inside a network
//     (localhost, *.internal, single-label names).
//   - Every address a name resolves to must be public. If any is not, the
//     fetch is refused (a name answering both public and private addresses
//     is either misconfigured or an attack).
//   - The connection goes to the address that was checked, never to the name:
//     the check runs inside the transport's dialer, once per connection, and
//     the dialer connects to the vetted IP. DNS can't change its answer
//     between the check and the connect (rebinding), and every redirect hop
//     opens a new connection that is checked the same way.
//   - Bounded: a size cap on the body, a deadline on the whole call, a cap on
//     redirects, and no proxy (a proxy would resolve the name itself).
//
// A box with no direct route to the internet simply gets no previews: the
// callers fail quietly to a plain link.
package safefetch

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

var (
	// ErrScheme is a link that isn't http or https.
	ErrScheme = errors.New("safefetch: only http and https links are fetched")
	// ErrUserInfo is a link carrying a user name or password.
	ErrUserInfo = errors.New("safefetch: links with a user name or password are not fetched")
	// ErrPort is a port other than the usual web ones.
	ErrPort = errors.New("safefetch: that port is not fetched")
	// ErrHost is a host name that only means something inside a network, or
	// one that isn't a valid name.
	ErrHost = errors.New("safefetch: that host is not fetched")
	// ErrBlocked is an address inside the network: private, loopback,
	// link-local, metadata and the other special ranges.
	ErrBlocked = errors.New("safefetch: that address is inside the network")
	// ErrResolvesInside is a NAME whose address is inside the network. It is
	// ErrBlocked too (errors.Is), but a caller that tells people why a fetch
	// failed says "couldn't reach it" for this one, as for a name with no
	// address: saying "inside the network" would let anyone who can ask for a
	// preview learn which names the server's own DNS knows.
	ErrResolvesInside = fmt.Errorf("%w (the name points there)", ErrBlocked)
	// ErrNoAddress is a name with no address.
	ErrNoAddress = errors.New("safefetch: the host has no address")
	// ErrRedirects is a chain of redirects longer than allowed.
	ErrRedirects = errors.New("safefetch: too many redirects")
)

// allowedPorts are the ports a fetch may use: the web's, and the two common
// alternatives. A link to any other port (22, 25, 6379...) is refused before
// anything is resolved: those are services, not pages.
var allowedPorts = map[int]bool{80: true, 443: true, 8080: true, 8443: true}

// blockedSuffixes are name endings that only resolve inside a network.
var blockedSuffixes = []string{".localhost", ".local", ".internal", ".home.arpa", ".lan", ".intranet", ".corp", ".private"}

// blockedNames are whole names that are never fetched.
var blockedNames = map[string]bool{"localhost": true, "metadata": true}

// v4Blocked are the IPv4 ranges a fetch never reaches (RFC 6890 and friends).
// Public addresses are everything global-unicast outside these.
var v4Blocked = mustPrefixes(
	"0.0.0.0/8",          // "this network"; 0.0.0.0 reaches the host itself
	"10.0.0.0/8",         // private
	"100.64.0.0/10",      // carrier-grade NAT, and Alibaba's metadata (100.100.100.200)
	"127.0.0.0/8",        // loopback
	"169.254.0.0/16",     // link-local, and every cloud's metadata (169.254.169.254)
	"172.16.0.0/12",      // private
	"192.0.0.0/24",       // IETF protocol assignments, and Oracle's metadata (192.0.0.192)
	"192.0.2.0/24",       // documentation
	"192.88.99.0/24",     // 6to4 relay anycast
	"192.168.0.0/16",     // private
	"198.18.0.0/15",      // benchmarking
	"198.51.100.0/24",    // documentation
	"203.0.113.0/24",     // documentation
	"224.0.0.0/4",        // multicast
	"240.0.0.0/4",        // reserved
	"255.255.255.255/32", // broadcast
)

// v6Blocked are the IPv6 ranges a fetch never reaches. Only 2000::/3 (global
// unicast) is considered at all; these are the special ranges inside it, plus
// the ones that carry an IPv4 address that could point back inside.
var v6Blocked = mustPrefixes(
	"2001::/23",     // IETF protocol assignments (Teredo 2001::/32 included)
	"2001:db8::/32", // documentation
	"2002::/16",     // 6to4: carries an IPv4 address
	"3fff::/20",     // documentation (RFC 9637)
)

// v6Mapped are prefixes outside 2000::/3 that embed an IPv4 address, refused
// outright: the deprecated IPv4-compatible form, and local-use NAT64, whose
// prefix length (and so where the IPv4 address sits) is the network's choice.
var v6Mapped = mustPrefixes(
	"::/96",          // IPv4-compatible (deprecated)
	"64:ff9b:1::/48", // local-use NAT64
)

// nat64 is NAT64's well-known prefix (RFC 6052): the last four bytes are an
// IPv4 address, and a NAT64 gateway connects to exactly that address. An
// IPv6-only box behind DNS64 sees every IPv4-only site this way, so the
// address is read out and judged as the IPv4 address it is: 64:ff9b::7f00:1
// is 127.0.0.1, and refused.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}

var globalUnicastV6 = netip.MustParsePrefix("2000::/3")

// PublicAddr reports whether a is an address a fetch may connect to: a public
// unicast address, never private, loopback, link-local, metadata, multicast or
// any other special-purpose range, in IPv4 or IPv6, an IPv4 address written
// as IPv6 (::ffff:127.0.0.1, or NAT64's 64:ff9b::7f00:1) included. Pure.
func PublicAddr(a netip.Addr) bool {
	if !a.IsValid() || a.Zone() != "" {
		return false
	}
	// ::ffff:a.b.c.d is the IPv4 address a.b.c.d to every socket API.
	if a.Is4In6() {
		a = a.Unmap()
	}
	if nat64.Contains(a) {
		b := a.As16()
		return PublicAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	if a.Is4() {
		for _, p := range v4Blocked {
			if p.Contains(a) {
				return false
			}
		}
		return true
	}
	for _, p := range v6Mapped {
		if p.Contains(a) {
			return false
		}
	}
	if !globalUnicastV6.Contains(a) {
		// Loopback, unspecified, link-local, unique-local (fc00::/7, which
		// holds AWS's fd00:ec2::254), site-local, multicast and the rest.
		return false
	}
	for _, p := range v6Blocked {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// legacyIPv4 parses s the way inet_aton does, which is how most C resolvers
// (and so glibc's getaddrinfo) read a host name made of numbers: one to four
// parts, each decimal, octal (a leading 0) or hex (0x), the last part filling
// the bytes that are left. "2130706433", "0177.0.0.1", "0x7f.1" and "127.1"
// are all 127.0.0.1. Such a host is treated as that address and never handed
// to a resolver. Pure.
func legacyIPv4(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, len(parts))
	for i, p := range parts {
		v, ok := atonPart(p)
		if !ok {
			return netip.Addr{}, false
		}
		vals[i] = v
	}
	// Every part but the last is one byte; the last fills the rest.
	var n uint64
	for i, v := range vals[:len(vals)-1] {
		if v > 0xff {
			return netip.Addr{}, false
		}
		n |= v << (8 * uint(3-i))
	}
	last := vals[len(vals)-1]
	if last >= 1<<(8*uint(5-len(vals))) {
		return netip.Addr{}, false
	}
	n |= last
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}), true
}

// atonPart reads one part of an inet_aton address. Pure.
func atonPart(p string) (uint64, bool) {
	if p == "" {
		return 0, false
	}
	base := 10
	switch {
	case len(p) > 1 && (p[:2] == "0x" || p[:2] == "0X"):
		base, p = 16, p[2:]
		if p == "" {
			// "0x" alone is 0 to inet_aton.
			return 0, true
		}
	case len(p) > 1 && p[0] == '0':
		base, p = 8, p[1:]
	}
	v, err := strconv.ParseUint(p, base, 32)
	if err != nil {
		return 0, false
	}
	return v, true
}

// looksNumeric reports whether every label of a host is made only of digits,
// or of a hex-looking 0x form: names a resolver might read as an address. Such
// a host is refused unless legacyIPv4 can read it (and then its address is
// checked), so nothing numeric reaches a resolver. Pure.
func looksNumeric(host string) bool {
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			continue
		}
		l := strings.ToLower(label)
		if strings.HasPrefix(l, "0x") {
			l = l[2:]
			if strings.Trim(l, "0123456789abcdef") != "" {
				return false
			}
			continue
		}
		if strings.Trim(l, "0123456789") != "" {
			return false
		}
	}
	return true
}
