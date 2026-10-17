package safefetch

// The same rules for API calls: a client for an endpoint someone typed, such
// as an MCP server or a remote agent, that sends a credential with its
// requests.
//
// It is the one address rule above (PublicAddr), the same dialler and the same
// name checks. A Policy can widen it in two ways, and no others: any port,
// because an API's port is part of its address; and this server's own network,
// for services an admin runs beside it (EndpointPolicy). Neither ever admits
// link-local, the clouds' metadata services or any other special range.

import (
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Policy widens what a client may reach. The zero value is Fetch's: public
// addresses on the web's ports.
type Policy struct {
	// AnyPort allows any port from 1 to 65535, for an API endpoint whose port
	// is part of its address. Fetch keeps to the web's ports.
	AnyPort bool
	// PrivateNetwork also allows this server's own network: loopback, the
	// private ranges (10/8, 172.16/12, 192.168/16, fc00::/7), the shared
	// address space Tailscale uses (100.64/10), and the names that only
	// resolve inside one (a Docker service name, *.internal), for services an
	// admin runs there (EndpointPolicy). Link-local, the clouds' metadata
	// addresses and every other special range stay refused.
	PrivateNetwork bool
	// Also is a further rule a caller adds for every address it is about to
	// connect to, given the host as written. It can only refuse.
	Also func(host string, a netip.Addr) error
}

func (p Policy) portAllowed(port int) bool {
	if p.AnyPort {
		return port > 0 && port <= 65535
	}
	return allowedPorts[port]
}

func (p Policy) addrAllowed(a netip.Addr) bool {
	return PublicAddr(a) || (p.PrivateNetwork && PrivateNetworkAddr(a))
}

// Allows reports whether p lets a client connect to a, for a caller that
// dials by its own means (a database driver, a provider's pooled transport)
// and checks each address it is about to connect to. The one rule: a public
// address, or this server's own network when p allows it. Pure.
func (p Policy) Allows(a netip.Addr) bool {
	return p.addrAllowed(a)
}

// EndpointPolicy is the one rule for an endpoint the server sends a credential
// to, an MCP server or a remote agent: any port; this server's own network
// only when an admin added it (an admin runs the box, and self-hosters run
// such services beside it); public addresses only when a member added it, so
// a member can't use one to look inside; never metadata or link-local.
func EndpointPolicy(addedByAdmin bool) Policy {
	return Policy{AnyPort: true, PrivateNetwork: addedByAdmin}
}

// The metadata services that sit inside ranges an admin's endpoint may reach,
// refused by name: AWS's IPv6 one inside fc00::/7, and Alibaba's inside the
// shared address space.
var (
	awsMetadataV6     = netip.MustParsePrefix("fd00:ec2::/32")
	alibabaMetadataV4 = netip.MustParsePrefix("100.100.100.200/32")
)

// sharedAddressSpace is 100.64.0.0/10 (RFC 6598), carrier-grade NAT's range,
// where Tailscale and similar private networks put their machines.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// PrivateNetworkAddr reports whether a is on this server's own network in the
// sense Policy.PrivateNetwork allows: loopback, a private range, or the shared
// address space a Tailscale network uses; never a metadata address. Pure.
func PrivateNetworkAddr(a netip.Addr) bool {
	if !a.IsValid() || a.Zone() != "" {
		return false
	}
	a = a.Unmap()
	if awsMetadataV6.Contains(a) || alibabaMetadataV4.Contains(a) {
		return false
	}
	return a.IsLoopback() || a.IsPrivate() || sharedAddressSpace.Contains(a)
}

// CheckEndpoint is CheckURL under p: the rules that need no lookup.
func CheckEndpoint(raw string, p Policy) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHost, err)
	}
	if _, err := checkParsed(u, p); err != nil {
		return nil, err
	}
	return u, nil
}

// NewClient is an HTTP client that connects only where p allows, checked as
// each connection is made, so a name that changes its answer and every
// redirect are covered. It never uses a proxy.
//
// It is for calls that carry a credential, so it follows a redirect only on
// the origin of the request it came from: the same scheme, host and port. A
// server that redirects elsewhere is answered with an error, never followed
// with the credential, which is bound to the address it was given for. A
// timeout of 0 means none: a streamed answer relies on its context.
func NewClient(p Policy, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            p.dial,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    10 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxIdleConns:           20,
		IdleConnTimeout:        90 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > DefaultMaxRedirects {
				return ErrRedirects
			}
			if !sameOrigin(via[0].URL, req.URL) {
				return ErrOffOrigin
			}
			_, err := checkParsed(req.URL, p)
			return err
		},
	}
}

// ErrOffOrigin is a redirect to another scheme, host or port, which a client
// carrying a credential never follows.
var ErrOffOrigin = fmt.Errorf("safefetch: the server redirected to another address, which isn't followed")

// sameOrigin compares two URLs' scheme, host and port, default ports filled in.
func sameOrigin(a, b *url.URL) bool {
	norm := func(u *url.URL) (string, string, string) {
		scheme := strings.ToLower(u.Scheme)
		port := u.Port()
		if port == "" {
			if scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		return scheme, strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), port
	}
	as, ah, ap := norm(a)
	bs, bh, bp := norm(b)
	return as == bs && ah == bh && ap == bp
}
