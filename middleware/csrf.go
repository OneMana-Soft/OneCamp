package middleware

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/authcookie"
)

// CSRF middleware implements the double-submit cookie pattern, which
// fits this app's auth model (cookie-based JWT, browser frontend,
// SameSite=None for cross-subdomain support).
//
// How it works:
//
//  1. On every request the middleware ensures the client has a
//     `X-CSRF-Token` cookie. If missing/expired, it sets a fresh
//     random value (signed by HKDF over a server secret would be
//     stricter; the random-value variant is what the docs recommend
//     for double-submit).
//  2. On state-changing methods (POST/PUT/PATCH/DELETE), the request
//     MUST also send the same value back in the `X-CSRF-Token`
//     header. If absent or mismatched the request is rejected 403.
//  3. Origin/Referer is checked for state-changing methods as a
//     belt-and-braces guard against attackers who can set arbitrary
//     cookies (rare; normally only via a separate XSS vector).
//
// Bypasses:
//   - Public endpoints carrying their own auth (webhook signature,
//     OAuth state, signed unsubscribe token) skip CSRF — the
//     mounted route group simply doesn't include this middleware.
//   - GET/HEAD/OPTIONS are passthrough since they're idempotent.
//
// The middleware deliberately reads the cookie and the header — it
// does NOT consult the request body or query string. This keeps it
// compatible with multipart uploads (Slack export ZIP) and JSON APIs.
//
// Configuration:
//
//	CSRF_DISABLED=true            disables enforcement (dev only).
//	CSRF_ALLOWED_ORIGINS=...      CSV of additional allowed Origins.
//	                              FE_HOST_DOMAIN/BACKEND_DOMAIN are
//	                              already accepted automatically.
const (
	csrfCookieName = "X-CSRF-Token"
	csrfHeaderName = "X-CSRF-Token"
	csrfTokenBytes = 32
	csrfCookieTTL  = 24 * time.Hour
)

// CSRFMiddleware enforces double-submit-cookie CSRF on state-changing
// requests. Pure passthrough on safe methods.
func CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isCSRFDisabled() {
			ensureCSRFCookie(w, r)
			next.ServeHTTP(w, r)
			return
		}

		// Always set / refresh the cookie so the FE can read its value
		// for subsequent state-changing requests. The check below only
		// fires for state-changing methods.
		ensureCSRFCookie(w, r)

		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		// Origin / Referer cross-check. Either header must match a
		// trusted origin. Modern browsers always send Origin on
		// state-changing requests; a missing one indicates a script
		// outside the browser sandbox (curl, server-to-server) and is
		// rejected here.
		if !isTrustedOrigin(r) {
			helpers.LogWarnWithContext(r.Context(),
				"CSRF blocked: untrusted origin %s referer %s for %s %s",
				r.Header.Get("Origin"), r.Header.Get("Referer"), r.Method, r.URL.Path)
			http.Error(w, "csrf origin check failed", http.StatusForbidden)
			return
		}

		headerVal := strings.TrimSpace(r.Header.Get(csrfHeaderName))
		if headerVal == "" || !csrfHeaderMatchesAnyCookie(r, headerVal) {
			helpers.LogWarnWithContext(r.Context(),
				"CSRF blocked: cookie/header mismatch for %s %s", r.Method, r.URL.Path)
			http.Error(w, "csrf token missing or invalid", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func isCSRFDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CSRF_DISABLED")))
	return v == "1" || v == "true" || v == "yes"
}

// ensureCSRFCookie guarantees the client holds a `X-CSRF-Token` cookie
// scoped to the shared parent domain (FE_DOMAIN) so the cross-subdomain
// FE can read it.
//
// It is deliberately written on EVERY request (not just when absent):
//   - If a token already exists we reuse its value, so the token stays
//     stable across the session and in-flight double-submit checks
//     never see a changing value.
//   - Re-writing with the correct Domain heals the migration case where
//     an older deploy set a host-only cookie on the BE subdomain that
//     the FE could never read. Reusing the same value also collapses
//     the transient "two cookies, same name" ambiguity (host-only +
//     domain) into a single matching value.
func ensureCSRFCookie(w http.ResponseWriter, r *http.Request) {
	tok := readCookie(r, csrfCookieName)
	if tok == "" {
		minted, err := generateCSRFToken()
		if err != nil {
			// Token generation should never fail on a healthy box. Log
			// and proceed without the cookie; the next request will try
			// again.
			helpers.LogErrorWithContext(r.Context(), "CSRF token mint failed: %+v", err)
			return
		}
		tok = minted
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    tok,
		Path:     "/",
		Expires:  time.Now().Add(csrfCookieTTL),
		Secure:   csrfCookieSecure(),
		SameSite: csrfCookieSameSite(),
		// Domain is set to the shared parent (FE_DOMAIN, e.g.
		// "onemana.dev") so the cookie is visible to the FE origin,
		// which lives on a *different subdomain* than the BE
		// (onecamp.onemana.dev vs onecamp-backend.onemana.dev).
		//
		// Without this the cookie is host-only on the BE subdomain and
		// the browser never exposes it to the FE's document.cookie, so
		// the FE cannot echo it back in the X-CSRF-Token header and
		// every state-changing request fails the double-submit check.
		// This mirrors authcookie.Set, which scopes the auth cookies
		// the same way.
		Domain: authcookie.FrontendDomain(),
		// HttpOnly is intentionally false — the FE must read the
		// cookie to echo it back as a header.
	})
}

func generateCSRFToken() (string, error) {
	buf := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func readCookie(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// csrfHeaderMatchesAnyCookie reports whether the submitted header value
// equals ANY cookie named csrfCookieName on the request.
//
// net/http's r.Cookie(name) returns only the first cookie with a given
// name, but a browser can legitimately present two same-named cookies
// during a deploy that changes the cookie's Domain scope: the old
// host-only cookie (BE subdomain) and the new parent-domain cookie may
// coexist until the old one expires. We accept a match against either
// so the double-submit check doesn't spuriously 403 mid-migration. The
// comparison stays constant-time per candidate.
func csrfHeaderMatchesAnyCookie(r *http.Request, headerVal string) bool {
	matched := false
	for _, c := range r.Cookies() {
		if c.Name != csrfCookieName {
			continue
		}
		if constantTimeEqualString(c.Value, headerVal) {
			matched = true
		}
	}
	return matched
}

func constantTimeEqualString(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func isTrustedOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	referer := strings.TrimSpace(r.Header.Get("Referer"))
	if origin == "" && referer == "" {
		return false
	}
	for _, allowed := range trustedOrigins() {
		if allowed == "" {
			continue
		}
		if origin != "" && strings.EqualFold(origin, allowed) {
			return true
		}
		if referer != "" && strings.HasPrefix(strings.ToLower(referer), strings.ToLower(allowed)) {
			return true
		}
	}
	return false
}

func trustedOrigins() []string {
	out := []string{}
	if fe := os.Getenv("FE_HOST_DOMAIN"); fe != "" {
		out = append(out, "https://"+fe, "http://"+fe)
	}
	if be := os.Getenv("BACKEND_DOMAIN"); be != "" {
		out = append(out, "https://"+be, "http://"+be)
	}
	if extra := os.Getenv("CSRF_ALLOWED_ORIGINS"); extra != "" {
		for _, p := range strings.Split(extra, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
	}
	// Dev convenience.
	out = append(out,
		"http://localhost:3000", "http://localhost:3001",
		"https://localhost:3000", "https://localhost:3001",
	)
	return out
}

func csrfCookieSecure() bool {
	secure, _ := authcookie.SecureAndSameSite()
	return secure
}

func csrfCookieSameSite() http.SameSite {
	_, sameSite := authcookie.SecureAndSameSite()
	return sameSite
}
