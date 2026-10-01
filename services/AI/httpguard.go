package ai

// Hardened HTTP client construction for AI providers.
//
// Threat model (single-tenant, self-hosted): an admin can point a custom
// "OpenAI-compatible" provider at an arbitrary base URL. The Go service
// then makes server-side requests to it. That is a classic SSRF surface.
//
// We deliberately do NOT block all private/internal addresses, because
// the PRIMARY use case is reaching internal model servers (Ollama at
// http://ollama:11434, a vLLM pod on the cluster network, etc.). Blanket
// private-IP blocking would defeat the product.
//
// What we DO block, at dial time (so DNS-rebinding can't bypass it), is
// the cloud instance-metadata service and link-local ranges — the only
// "private" targets that are pure attack value with no legitimate model
// use. This is enforced only for admin-supplied custom endpoints; the
// built-in providers (Ollama via OLLAMA_HOST, api.openai.com,
// api.anthropic.com) are trusted and skip the guard.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
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

// isLocalAIDialIP reports whether an IP is on the customer's own infrastructure
// (loopback / private / link-local) — the only targets permitted under
// local-only AI mode.
func isLocalAIDialIP(ip net.IP) bool {
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// errBlockedAddress is returned by the guarded dialer for denied targets.
var errBlockedAddress = errors.New("ai: connection to this address is blocked for security (cloud metadata / link-local)")

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

// blockedCIDRs are ranges with no legitimate model-server use but high
// SSRF value. We block link-local (which contains the cloud metadata
// IPs) for both IPv4 and IPv6. Private/ULA ranges are intentionally NOT
// here — self-hosted model servers live there.
var blockedCIDRs = func() []*net.IPNet {
	cidrs := []string{
		"169.254.0.0/16", // IPv4 link-local — includes 169.254.169.254 (AWS/GCP/Azure metadata)
		"fe80::/10",      // IPv6 link-local
		"fd00:ec2::/32",  // AWS IMDS over IPv6 (fd00:ec2::254)
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// isBlockedIP reports whether an IP must never be dialed by a custom
// endpoint. It always blocks link-local/metadata ranges and the
// unspecified address.
//
// When AI_BLOCK_PRIVATE_IPS is enabled, it additionally blocks loopback,
// RFC1918 private ranges, and IPv6 ULA — i.e. anything not globally
// routable. Use this when you want to prevent a workspace admin from
// probing the host's private network via a custom endpoint. The
// self-hosted default leaves those reachable so internal model servers
// (Ollama, vLLM on the same host/VPC) work.
func isBlockedIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() {
		return true
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	for _, n := range blockedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	if blockPrivateIPs() {
		if ip.IsLoopback() || ip.IsPrivate() {
			return true
		}
	}
	return false
}

// blockPrivateIPs reports whether the strict policy (block all
// non-public addresses) is enabled via env. Default false for the
// self-hosted product so same-host/VPC model servers stay reachable.
func blockPrivateIPs() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AI_BLOCK_PRIVATE_IPS")))
	return v == "1" || v == "true" || v == "yes"
}
