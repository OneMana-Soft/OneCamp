package helpers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ValidateOutboundURL checks that a URL is safe for server-side HTTP requests.
// It enforces HTTPS (outside dev), blocks loopback/link-local/private/multicast,
// and prevents redirect-based SSRF via a custom CheckRedirect.
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

	// Reject plain IP literals that are unsafe
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
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

func isBlockedIP(ip net.IP) bool {
	// Loopback
	if ip.IsLoopback() {
		return true
	}
	// Link-local
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	// Private / RFC1918 + IPv6 ULA
	if ip.IsPrivate() {
		return true
	}
	// Multicast
	if ip.IsMulticast() {
		return true
	}
	return false
}

// SSRFSafeClient returns an *http.Client that blocks redirects to blocked IPs.
func SSRFSafeClient(allowHTTPInDev bool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				// Re-resolve and block at dial time to prevent DNS rebinding
				ips, err := net.LookupIP(host)
				if err != nil {
					return nil, fmt.Errorf("SSRF dial lookup failed: %w", err)
				}
				for _, ip := range ips {
					if isBlockedIP(ip) {
						return nil, fmt.Errorf("SSRF dial blocked IP for host %s: %s", host, ip)
					}
				}
				// Pin the first safe IP to close the TOCTOU window
				dialer := &net.Dialer{Timeout: 10 * time.Second}
				return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
			},
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
