package business

// hostGuard.go — the DESTINATION POLICY for external data sources.
//
// Why this exists: configuring a data source (including "test connection") is
// capability-gated with agent.manage, which an admin may delegate to ordinary
// members (see router.go). A connection test reports whether an arbitrary
// host:port answered, so without a destination policy that endpoint is an
// internal-network probe: loopback, RFC1918/ULA, link-local and the cloud
// instance-metadata endpoint are all reachable from the server.
//
// The policy is engine-generic on purpose — it only reasons about host, IP and
// port — so Postgres, MySQL and any engine added later get it for free:
//
//  1. HARD DENY, not overridable by configuration: link-local (which contains
//     169.254.169.254, the AWS/GCP/Azure metadata endpoint), the known
//     non-link-local cloud metadata addresses, multicast, the unspecified
//     address and the IPv4 broadcast address. Nothing legitimate listens there;
//     credential theft does.
//  2. INTERNAL (loopback, RFC1918 / IPv6-ULA private, CGNAT shared space):
//     denied UNLESS explicitly allowlisted. A self-hosted install that really
//     does keep its database on the LAN keeps working — through configuration
//     (AI_DATASOURCE_HOST_ALLOWLIST), instead of leaving the whole private
//     network open to everyone holding agent.manage.
//  3. PUBLIC: allowed when no allowlist is configured; once an allowlist IS
//     configured it is exhaustive and every destination must match it.
//
// Names are resolved with a bounded DNS timeout and EVERY returned address is
// checked, failing closed (a name resolving to one public and one private
// address is refused, because we cannot control which answer the driver picks).
// The same check runs again inside guardedDialContext at the moment of
// connecting, which then dials the exact address it just validated — so a DNS
// answer that flips to a blocked address between validation and connect (DNS
// rebinding) does not get us to connect there.

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

const (
	// hostAllowlistEnv is the operator-facing allowlist. Comma-separated, and
	// each entry may be an exact hostname, an exact IP, or a CIDR range.
	hostAllowlistEnv = "AI_DATASOURCE_HOST_ALLOWLIST"
	// dnsTimeout bounds name resolution so a slow/black-holed resolver cannot
	// hold a request open.
	dnsTimeout = 3 * time.Second
	// guardedDialTimeout bounds one TCP connect attempt (the drivers' own
	// connect timeouts do not apply to a custom dialer).
	guardedDialTimeout = connectTimeoutSecs * time.Second
)

// hardDenyCIDRs are destinations no configuration may enable.
var hardDenyCIDRs = mustCIDRs(
	"169.254.0.0/16",     // IPv4 link-local — includes 169.254.169.254 (AWS/GCP/Azure IMDS)
	"fe80::/10",          // IPv6 link-local
	"fd00:ec2::/32",      // AWS IMDS over IPv6
	"192.0.0.192/32",     // Oracle Cloud metadata
	"100.100.100.200/32", // Alibaba Cloud metadata
)

// internalCIDRs are on-infrastructure ranges that net.IP's own predicates do not
// cover; like loopback and private they need an explicit allowlist entry.
var internalCIDRs = mustCIDRs(
	"100.64.0.0/10", // CGNAT / shared address space (carrier + k8s node networks)
)

func mustCIDRs(list ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(list))
	for _, c := range list {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// ───────────────────────── allowlist ─────────────────────────

// allowEntry is one parsed AI_DATASOURCE_HOST_ALLOWLIST entry. Exactly one of
// the three forms is set, which is what lets a single matcher serve all of them.
type allowEntry struct {
	host string     // exact hostname, lowercased
	ip   net.IP     // exact IP literal
	cidr *net.IPNet // CIDR range
}

// matches is THE allowlist matcher, shared by the pre-connect check and the
// dial-time check: an entry matches when the literal hostname is equal
// (case-insensitive), the exact IP is equal, or the IP is inside the CIDR.
// ip may be nil to ask "is this NAME allowlisted".
func (e allowEntry) matches(host string, ip net.IP) bool {
	switch {
	case e.cidr != nil:
		return ip != nil && e.cidr.Contains(ip)
	case e.ip != nil:
		return ip != nil && e.ip.Equal(ip)
	default:
		return e.host != "" && e.host == normalizeHost(host)
	}
}

// allowlistMatches reports whether any entry allows this (host, ip) pair.
func allowlistMatches(entries []allowEntry, host string, ip net.IP) bool {
	for _, e := range entries {
		if e.matches(host, ip) {
			return true
		}
	}
	return false
}

// parseAllowlist turns the raw env value into entries, classifying each token as
// CIDR / IP / hostname. Unparseable-as-address tokens are treated as hostnames.
func parseAllowlist(raw string) []allowEntry {
	var out []allowEntry
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(p); err == nil {
			out = append(out, allowEntry{cidr: n})
			continue
		}
		if ip := net.ParseIP(strings.Trim(p, "[]")); ip != nil {
			out = append(out, allowEntry{ip: ip})
			continue
		}
		out = append(out, allowEntry{host: normalizeHost(p)})
	}
	return out
}

// hostAllowlist reads the current allowlist. Read per call (not cached) so an
// operator's change takes effect on restart-free config reloads and so tests
// stay hermetic.
func hostAllowlist() []allowEntry {
	return parseAllowlist(os.Getenv(hostAllowlistEnv))
}

func normalizeHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

// ───────────────────────── policy errors ─────────────────────────

// destinationError is a policy refusal. Its text is written here — never driver
// text — so the controller can return it verbatim, and it stays recognizable
// through errors.As even after a driver hands it back from a dial.
type destinationError struct{ msg string }

func (e *destinationError) Error() string { return e.msg }

func denyf(format string, a ...interface{}) error {
	return &destinationError{msg: fmt.Sprintf(format, a...)}
}

// ───────────────────────── IP classification ─────────────────────────

// hardDeniedIP reports whether an address is refused regardless of allowlist,
// plus a human reason for the refusal message.
func hardDeniedIP(ip net.IP) (string, bool) {
	switch {
	case ip.IsUnspecified():
		return "the unspecified address", true
	case ip.IsMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsLinkLocalMulticast():
		return "a multicast address", true
	case ip.IsLinkLocalUnicast():
		return "a link-local address (cloud instance metadata lives there)", true
	case ip.Equal(net.IPv4bcast):
		return "the broadcast address", true
	}
	for _, n := range hardDenyCIDRs {
		if n.Contains(ip) {
			return "a cloud instance-metadata address", true
		}
	}
	return "", false
}

// internalIP reports whether an address is on the install's own infrastructure
// and therefore needs an explicit allowlist entry.
func internalIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	for _, n := range internalCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ───────────────────────── checks ─────────────────────────

// checkHostDestination applies the policy to a configured host before a
// connector is built, so create/update/test fail fast with an actionable message
// instead of a driver timeout. Shared by create/update validation and the
// pre-save connection test, so neither can open a host the other would refuse.
func checkHostDestination(host string) error {
	_, err := resolveAllowedAddrs(context.Background(), host)
	return err
}

// resolveAllowedAddrs resolves host (IP literal or DNS name) under a bounded
// timeout and returns the addresses that PASS the policy. Fails closed: one bad
// address refuses the whole host.
func resolveAllowedAddrs(ctx context.Context, host string) ([]net.IP, error) {
	h := strings.Trim(strings.TrimSpace(host), "[]")
	if h == "" {
		return nil, denyf("host is required")
	}
	if strings.HasPrefix(h, "/") {
		return nil, denyf("a unix socket path is not a valid data source host")
	}

	entries := hostAllowlist()
	// A hostname entry authorizes the NAME itself: that is how an operator opts
	// in an internal database whose address is dynamic (container/service DNS).
	nameAllowed := allowlistMatches(entries, h, nil)

	ips, err := lookupHostIPs(ctx, h)
	if err != nil {
		return nil, err
	}

	out := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if reason, denied := hardDeniedIP(ip); denied {
			return nil, denyf("host %q resolves to %s, which is never allowed for a data source", host, reason)
		}
		allowed := nameAllowed || allowlistMatches(entries, h, ip)
		if internalIP(ip) && !allowed {
			return nil, denyf("host %q resolves to an internal address; add the host, IP, or CIDR to %s to use an internal database", host, hostAllowlistEnv)
		}
		if len(entries) > 0 && !allowed {
			return nil, denyf("host %q is not in the allowed data-source host list (%s)", host, hostAllowlistEnv)
		}
		out = append(out, ip)
	}
	if len(out) == 0 {
		return nil, denyf("host %q resolved to no usable addresses", host)
	}
	return out, nil
}

// lookupHostIPs returns the literal IP or every DNS answer for a name, with a
// bounded resolver timeout.
func lookupHostIPs(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	lctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(lctx, host)
	if err != nil {
		return nil, denyf("could not resolve data source host %q", host)
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if a.IP != nil {
			out = append(out, a.IP)
		}
	}
	if len(out) == 0 {
		return nil, denyf("data source host %q resolved to no addresses", host)
	}
	return out, nil
}

// ───────────────────────── guarded dialing ─────────────────────────

// guardedDialContext is the dial hook both engines' drivers use. It re-applies
// the policy at connect time and then dials the exact address it validated, so a
// name whose DNS answer changes after validation cannot steer the connection to
// a blocked address (DNS rebinding). This is also the backstop for sources saved
// before the policy existed: every connection goes through here, not just
// create/update/test.
func guardedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if !strings.HasPrefix(network, "tcp") {
		return nil, denyf("a data source may only be reached over tcp")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, denyf("invalid data source address")
	}
	ips, err := resolveAllowedAddrs(ctx, host)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, guardedDialTimeout)
	defer cancel()
	var d net.Dialer
	var lastErr error
	for _, ip := range ips {
		conn, derr := d.DialContext(dctx, network, net.JoinHostPort(ip.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no usable address for data source host")
	}
	return nil, lastErr
}

// pqGuardedDialer adapts guardedDialContext to lib/pq's dialer interfaces. pq
// prefers DialContext when the dialer implements DialerContext, so the request
// context (and its deadline) flows through.
type pqGuardedDialer struct{}

func (pqGuardedDialer) Dial(network, address string) (net.Conn, error) {
	return guardedDialContext(context.Background(), network, address)
}

func (pqGuardedDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return guardedDialContext(ctx, network, address)
}

func (pqGuardedDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return guardedDialContext(ctx, network, address)
}
