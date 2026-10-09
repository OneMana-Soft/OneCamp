package helpers

import (
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
)

// ClientIP is the address a request came from, for rate limits and the audit
// log.
//
// WHOSE HEADERS TO BELIEVE. The backend has no published port: requests reach
// it through Traefik, or from another service on the same private network. So
// when the request's own peer is a private address, the forwarding headers
// are the proxy's and are believed; from any other peer (a backend someone
// exposed directly) they are whatever the sender wrote, and the peer is the
// client. TRUST_PROXY_HEADERS=false ignores the headers from every peer;
// =true believes them from every peer. Publishing the backend's port on the
// host breaks the default (Docker hands those connections over from its
// bridge, a private address): set false then. (Until 9 Oct 2026 the headers were
// believed only with TRUST_PROXY_HEADERS=true, which no shipped config set,
// so every request had Traefik's address and every per-address limit was one
// limit for the whole workspace: twenty bad sign-ins locked everyone out.)
//
// WHICH ENTRY. The right-most X-Forwarded-For entry, across every header
// line: the address the nearest proxy saw. Anything to its left was written
// by whoever sent the request; the left-most entry, which was used before, is
// exactly that, so rotating it escaped every per-address limit.
//
// CLOUDFLARE. When that address is one of Cloudflare's, the request came
// through Cloudflare, which thousands of visitors share an address of, and
// the visitor is in CF-Connecting-IP. That header is believed only then, so a
// request that didn't come through Cloudflare can't choose its address with
// it.
func ClientIP(r *http.Request) string {
	peer := hostOf(r.RemoteAddr)
	if !trustForwarded(peer) {
		return peer
	}
	hop, ok := lastForwarded(r)
	if !ok {
		real, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP")))
		if err != nil {
			return peer
		}
		hop = real.Unmap()
	}
	if isCloudflare(hop) {
		if visitor, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); err == nil {
			return visitor.Unmap().String()
		}
	}
	return hop.String()
}

func hostOf(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// trustForwarded says whether a request's forwarding headers are believed,
// given its peer.
func trustForwarded(peer string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TRUST_PROXY_HEADERS"))) {
	case "false":
		return false
	case "true":
		return true
	}
	ip, err := netip.ParseAddr(peer)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

// lastForwarded is the right-most address in X-Forwarded-For, over every line
// of the header.
func lastForwarded(r *http.Request) (netip.Addr, bool) {
	lines := r.Header.Values("X-Forwarded-For")
	for i := len(lines) - 1; i >= 0; i-- {
		entries := strings.Split(lines[i], ",")
		for j := len(entries) - 1; j >= 0; j-- {
			e := strings.TrimSpace(entries[j])
			if e == "" {
				continue
			}
			ip, err := netip.ParseAddr(e)
			if err != nil {
				// The nearest proxy's entry isn't an address: nothing to its
				// left can be believed instead.
				return netip.Addr{}, false
			}
			return ip.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

// cloudflareRanges is where Cloudflare's edge connects from, as published at
// https://www.cloudflare.com/ips-v4/ and /ips-v6/ (read 9 Oct 2026). They
// change rarely; a range missing here only means a visitor behind it is
// counted by Cloudflare's address, as everyone was before.
var cloudflareRanges = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22", "141.101.64.0/18",
		"108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20", "197.234.240.0/22", "198.41.128.0/17",
		"162.158.0.0/15", "104.16.0.0/13", "104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
		"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32", "2405:8100::/32",
		"2a06:98c0::/29", "2c0f:f248::/32",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

func isCloudflare(ip netip.Addr) bool {
	for _, p := range cloudflareRanges {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
