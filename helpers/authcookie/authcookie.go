// Package authcookie centralises the construction of the auth cookies
// (`Authorization`, `RefreshToken`, `DeviceId`).
//
// Why this matters:
//   - Every cookie that carries a session token must be HttpOnly so a
//     successful XSS cannot exfiltrate it.
//   - Every cookie that carries a session token must be Secure when
//     served over HTTPS to prevent leakage to a network attacker.
//   - SameSite policy follows the same logic as the existing
//     getCookieSecureAndSameSite helper in controllers/User: lax in
//     dev (HTTP), none in production (cross-subdomain HTTPS).
//
// Centralising the cookie construction ensures we never miss the
// HttpOnly flag again — every call site composes a single struct
// rather than writing five identical SetCookie blocks.
package authcookie

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"net"
)

// Auth holds the timestamps for the three auth cookies. Pass it to
// Set() to write all three in one call.
type Auth struct {
	AuthToken        string
	AuthExpiresAt    time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	DeviceID         string
	DeviceExpiresAt  time.Time
}

// Set writes the three auth cookies to w with the appropriate
// security flags. Idempotent and safe to call from any handler that
// produces a session.
func Set(w http.ResponseWriter, a Auth) {
	secure, sameSite := SecureAndSameSite()
	domain := FrontendDomain()

	http.SetCookie(w, &http.Cookie{
		Name:     "Authorization",
		Value:    a.AuthToken,
		Expires:  a.AuthExpiresAt,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   domain,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "RefreshToken",
		Value:    a.RefreshToken,
		Expires:  a.RefreshExpiresAt,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   domain,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "DeviceId",
		Value:    a.DeviceID,
		Expires:  a.DeviceExpiresAt,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   domain,
	})
}

// FrontendDomain returns the cookie Domain attribute. Mirrors the
// existing getFrontendCookieDomain in controllers/User but lives here
// so other packages can reuse it.
func FrontendDomain() string {
	raw := strings.TrimSpace(os.Getenv("FE_DOMAIN"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("FRONTEND_DOMAIN"))
	}
	if raw == "" {
		return ""
	}
	// Strip scheme if the env var contains a full URL (e.g. https://app.example.com).
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
			raw = parsed.Host
		}
	}
	if strings.Contains(raw, ":") {
		if host, _, err := net.SplitHostPort(raw); err == nil {
			raw = host
		} else {
			parts := strings.Split(raw, ":")
			if len(parts) > 0 {
				raw = parts[0]
			}
		}
	}
	return raw
}

// SecureAndSameSite returns (secure, sameSite). Logic mirrors the
// existing controllers helper to preserve dev-HTTP behaviour: when
// COOKIE_SECURE=false (or hostname looks like localhost) we drop
// Secure and use SameSite=Lax so browsers accept the cookie.
func SecureAndSameSite() (bool, http.SameSite) {
	if strings.TrimSpace(strings.ToLower(os.Getenv("COOKIE_SECURE"))) == "false" {
		return false, http.SameSiteLaxMode
	}
	feDomain := strings.ToLower(strings.TrimSpace(os.Getenv("FE_DOMAIN")))
	if feDomain == "" {
		feDomain = strings.ToLower(strings.TrimSpace(os.Getenv("FRONTEND_DOMAIN")))
	}
	if feDomain == "" {
		return true, http.SameSiteNoneMode
	}
	if strings.Contains(feDomain, "localhost") || strings.HasPrefix(feDomain, "127.0.0.1") {
		return false, http.SameSiteLaxMode
	}
	return true, http.SameSiteNoneMode
}
