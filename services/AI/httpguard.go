package ai

// Hardened HTTP client construction for AI providers.
//
// Threat model (single-tenant, self-hosted): an admin can point a custom
// "OpenAI-compatible" provider, the web-search tool or model discovery at an
// arbitrary base URL, and the Go service then makes server-side requests to
// it. That is a classic SSRF surface.
//
// THE ONE ADDRESS RULE (safefetch.EndpointPolicy) decides what such a client
// may reach, as it does for MCP servers and remote agents: these endpoints are
// set by an admin, so this server's own network (Ollama at
// http://ollama:11434, a vLLM pod on the cluster network, a box on Tailscale)
// is reachable, while link-local, the clouds' metadata services and every
// other special range are not. AI_BLOCK_PRIVATE_IPS narrows it to public
// addresses only, for an operator who wants no custom endpoint inside. It is
// enforced at dial time, on the address actually connected to, so DNS
// rebinding can't bypass it. The built-in providers (Ollama via OLLAMA_HOST,
// api.openai.com, api.anthropic.com) are trusted and skip the guard.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/akashc777/OneCamp/helpers/safefetch"
)

// httpClientConfig configures a provider's HTTP client.
type httpClientConfig struct {
	// timeout is the whole-request timeout. 0 = no timeout (used for
	// streaming endpoints, which rely on context cancellation instead).
	timeout time.Duration
	// insecureSkipVerify disables TLS certificate verification. Opt-in,
	// per-provider, for self-hosted endpoints using self-signed certs.
	insecureSkipVerify bool
	// guardSSRF enables the dial-time metadata/link-local denylist. Set
	// for admin-supplied custom endpoints; false for trusted built-ins.
	guardSSRF bool
	// offLocalRefusal, when non-empty, refuses any target that resolves
	// OUTSIDE the customer's own network, and is the sentence saying why.
	//
	// For a caller that has decided this particular connection is only safe on
	// the local network: a remote agent reached over plain HTTP, or one with no
	// credential. The decision belongs to the caller, which knows what it is
	// about to send; the enforcement belongs here, which is the only place that
	// knows the address actually dialled rather than the one that was typed.
	offLocalRefusal string
}

// localOnlyEnabled is the live local-only AI switch, read at DIAL time by every
// provider client so the "no content leaves to a cloud model" guarantee holds
// across all egress paths without rebuilding clients. Set from Config on
// service init/reload.
var localOnlyEnabled atomic.Bool

// SetLocalOnlyMode publishes the local-only switch. Called by NewAIService /
// ReloadAIService from Config.LocalOnlyMode.
func SetLocalOnlyMode(on bool) { localOnlyEnabled.Store(on) }

// errLocalOnly is returned by the dialer when local-only mode blocks a non-local target.
var errLocalOnly = errors.New("ai: local-only mode is on — refusing to send content to a non-local model endpoint")

// isLocalAIDialIP reports whether an IP is on the customer's own network in the
// one sense this server uses (safefetch.PrivateNetworkAddr): loopback, the
// private ranges and the shared address space Tailscale uses, never a cloud's
// metadata or link-local address. The only targets local-only AI mode permits,
// and where content that may not leave the network may go.
func isLocalAIDialIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	return ok && safefetch.PrivateNetworkAddr(a.Unmap())
}

// errBlockedAddress is returned by the guarded dialer for denied targets.
var errBlockedAddress = errors.New("ai: connection to this address is blocked for security (cloud metadata, link-local or another special address)")

// newProviderHTTPClient builds an *http.Client per the config. It is the
// single chokepoint for provider networking so timeouts, TLS, and the
// SSRF guard are applied consistently.
func newProviderHTTPClient(cfg httpClientConfig) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	if cfg.insecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 — opt-in for self-signed self-hosted endpoints
	}

	// Single guarded dialer for every provider client. At DIAL time it checks,
	// in order: (1) local-only mode (live switch) — permit only on-infra IPs;
	// (2) the per-client SSRF denylist for custom endpoints. The IP that is
	// checked is the IP that is dialed, closing the DNS-rebinding gap. When
	// neither guard applies it's a plain dial.
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		localOnly := localOnlyEnabled.Load()
		if !localOnly && !cfg.guardSSRF && cfg.offLocalRefusal == "" {
			return dialer.DialContext(ctx, network, addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no addresses for %s", host)
		}
		for _, ipAddr := range ips {
			if derr := dialAllowed(cfg, localOnly, host, ipAddr.IP); derr != nil {
				return nil, derr
			}
		}
		var lastErr error
		for _, ipAddr := range ips {
			conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no addresses for %s", host)
		}
		return nil, lastErr
	}

	return &http.Client{
		Timeout:   cfg.timeout,
		Transport: transport,
	}
}

// dialAllowed applies every rule that can refuse one resolved address, in
// order of how much the operator gave up to enable it.
//
// Pure, and deliberately separate from the dialer: these three rules are the
// egress policy of this whole service, and until they were lifted out of a
// closure the only way to exercise them was to make a real connection to a
// real address, which is to say they were not exercised.
func dialAllowed(cfg httpClientConfig, localOnly bool, host string, ip net.IP) error {
	// Local-only mode first: an operator who turned it on asked for nothing to
	// leave, and that outranks anything a per-client config permits.
	if localOnly && !isLocalAIDialIP(ip) {
		return fmt.Errorf("%w: %s", errLocalOnly, ip)
	}
	if cfg.guardSSRF && isBlockedIP(ip) {
		return fmt.Errorf("%w: %s", errBlockedAddress, ip)
	}
	if cfg.offLocalRefusal != "" && !isLocalAIDialIP(ip) {
		return fmt.Errorf("%s (%s resolves to %s)", cfg.offLocalRefusal, host, ip)
	}
	return nil
}

// isBlockedIP reports whether an IP must never be dialled by a custom endpoint:
// anything the one address rule refuses for an endpoint an admin added
// (safefetch.EndpointPolicy). Public addresses and this server's own network
// pass; link-local, the clouds' metadata services, the unspecified address and
// every other special range don't.
//
// When AI_BLOCK_PRIVATE_IPS is enabled, this server's own network is refused
// too, for an operator who wants no custom endpoint inside it.
func isBlockedIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	return !safefetch.EndpointPolicy(!blockPrivateIPs()).Allows(a.Unmap())
}

// blockPrivateIPs reports whether the strict policy (block all
// non-public addresses) is enabled via env. Default false for the
// self-hosted product so same-host/VPC model servers stay reachable.
func blockPrivateIPs() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AI_BLOCK_PRIVATE_IPS")))
	return v == "1" || v == "true" || v == "yes"
}
