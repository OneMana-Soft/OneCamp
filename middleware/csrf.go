package middleware

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/authcookie"
)

// CSRF middleware: the double-submit pattern, with the token in a cookie of
// the API host's own and echoed back by the web app in the X-CSRF-Token
// header. It fits this app's auth model (cookie sessions, a browser web app on
// a sibling host of the API).
//
// How it works:
//
//  1. Every request through it gets the token in force: the one in this host's
//     CSRF cookie, or a new one. The cookie is written again for another day
//     (host-only, __Host-, HttpOnly, Lax: authcookie.Write), and the answer
//     carries the token in its X-CSRF-Token header, which CORS exposes. That
//     is how the web app learns it; GET /auth/csrf answers with nothing else,
//     for a write before any read.
//  2. A state-changing request (POST/PUT/PATCH/DELETE) must come from a trusted
//     origin (its Origin, or without one its Referer's origin, both compared
//     exactly) and send the token in force back in the X-CSRF-Token header.
//     Otherwise 403.
//  3. A refusal carries the token in force too, so a web app whose token ran
//     out (a day idle) retries once with the new one.
//
// WHY THE TOKEN LEFT document.cookie (21 Oct 2026). The web app used to read
// it from a cookie on FE_DOMAIN, the parent of the web and API hosts. On
// managed Cloud that parent is the zone, so every workspace's token went to
// every other workspace's hosts, and any page on the zone could read it or
// write one of its own. Web builds from before then still read that cookie,
// and are kept working by csrfLegacy.go until it is removed.
//
// Bypasses:
//   - Public endpoints carrying their own auth (webhook signature, OAuth
//     state, signed unsubscribe token) skip CSRF: the mounted route group
//     simply doesn't include this middleware.
//   - GET/HEAD/OPTIONS are passthrough since they're idempotent.
//
// The middleware reads the cookie and the header only, never the body or the
// query string, so multipart uploads and JSON APIs work alike.
//
// Configuration:
//
//	CSRF_DISABLED=true            disables enforcement (dev only).
//	CSRF_ALLOWED_ORIGINS=...      CSV of additional allowed Origins.
//	                              FE_HOST_DOMAIN/BACKEND_DOMAIN are
//	                              already accepted automatically.
//	CSRF_LEGACY_COOKIE=off        stops the readable cookie kept for web
//	                              builds from before 21 Oct 2026.
const (
	csrfHeaderName = "X-CSRF-Token"
	csrfTokenBytes = 32
	csrfCookieTTL  = 24 * time.Hour
)

// CSRFMiddleware enforces double-submit CSRF on state-changing requests, and
// hands every request the token in force.
func CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ensureCSRFToken(w, r)
		keepLegacyCSRFCookie(w, r)

		if isCSRFDisabled() {
			next.ServeHTTP(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		// Modern browsers always send Origin on state-changing requests; a
		// missing one (and no Referer) is a script outside the browser
		// sandbox (curl, server-to-server) and is refused.
		if !isTrustedOrigin(r) {
			helpers.LogWarnWithContext(r.Context(),
				"CSRF blocked: untrusted origin %s referer %s for %s %s",
				r.Header.Get("Origin"), refererOrigin(r), r.Method, helpers.RouteOf(r))
			http.Error(w, "csrf origin check failed", http.StatusForbidden)
			return
		}

		header := strings.TrimSpace(r.Header.Get(csrfHeaderName))
		if header == "" || !(sameToken(header, token) || legacyCSRFMatches(r, header)) {
			helpers.LogWarnWithContext(r.Context(),
				"CSRF blocked: cookie/header mismatch for %s %s", r.Method, helpers.RouteOf(r))
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

// ensureCSRFToken returns the token in force and hands it out: in this host's
// cookie, written again for another day, and in the answer's X-CSRF-Token
// header. The token stays the same while its cookie lives, so a write in
// flight never meets a changed one. "" only when a new one couldn't be made: a
// write is then refused, and the next request tries again.
func ensureCSRFToken(w http.ResponseWriter, r *http.Request) string {
	token := ""
	if c, err := r.Cookie(authcookie.Name(authcookie.CSRF)); err == nil {
		token = c.Value
	}
	if token == "" {
		minted, err := generateCSRFToken()
		if err != nil {
			helpers.LogErrorWithContext(r.Context(), "CSRF token mint failed: %+v", err)
			return ""
		}
		token = minted
	}
	authcookie.Write(w, authcookie.CSRF, token, time.Now().Add(csrfCookieTTL))
	w.Header().Set(csrfHeaderName, token)
	return token
}

func generateCSRFToken() (string, error) {
	buf := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// sameToken compares in constant time; an empty token in force matches
// nothing.
func sameToken(header, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(header), []byte(token)) == 1
}

// isTrustedOrigin reports whether a state-changing request comes from a
// trusted origin. The Origin decides when there is one; a trusted Referer
// doesn't make up for an untrusted Origin. Without an Origin, the Referer's
// origin (scheme and host, port included) must be trusted, compared exactly:
// a prefix match let https://acme.onemana.dev.evil.example pass for
// https://acme.onemana.dev.
func isTrustedOrigin(r *http.Request) bool {
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		return originTrusted(origin)
	}
	referer := refererOrigin(r)
	return referer != "" && originTrusted(referer)
}

// refererOrigin is the origin (scheme and host) of r's Referer, "" when it has
// none. The check needs no more, and a log line takes no more: a Referer's
// path and query can carry a link's token.
func refererOrigin(r *http.Request) string {
	u, err := url.Parse(strings.TrimSpace(r.Header.Get("Referer")))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func originTrusted(origin string) bool {
	for _, allowed := range trustedOrigins() {
		if allowed != "" && strings.EqualFold(origin, allowed) {
			return true
		}
	}
	return false
}

// OwnOrigins are the web app's and the API's own origins (FE_HOST_DOMAIN,
// BACKEND_DOMAIN), and over plain http too only where cookies are insecure
// (development): on an https install a page over http is one anybody on the
// network path can write. Credentialed CORS (router) and the CSRF check both
// start from it, so the two can't trust different origins.
func OwnOrigins() []string {
	plainHTTP := !authcookie.Secure()
	out := []string{}
	for _, host := range []string{os.Getenv("FE_HOST_DOMAIN"), os.Getenv("BACKEND_DOMAIN")} {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		out = append(out, "https://"+host)
		if plainHTTP {
			out = append(out, "http://"+host)
		}
	}
	return out
}

// trustedOrigins are the CSRF check's: OwnOrigins, any listed in
// CSRF_ALLOWED_ORIGINS, and on a development backend the web's dev servers
// (DevOrigins).
func trustedOrigins() []string {
	out := OwnOrigins()
	if extra := os.Getenv("CSRF_ALLOWED_ORIGINS"); extra != "" {
		for _, p := range strings.Split(extra, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
	}
	return append(out, DevOrigins()...)
}

// DevOrigins are the web's local dev servers (next dev on 3000 and 3001),
// which credentialed CORS (router) and the CSRF check trust only on a
// development backend, where cookies are insecure. Every install trusted
// them, so a page on a visitor's own localhost:3000 passed both origin checks
// against a production API (audit, LOW). APP_ENV can't tell: no install sets
// it.
func DevOrigins() []string {
	if authcookie.Secure() {
		return nil
	}
	return []string{"http://localhost:3000", "http://localhost:3001", "https://localhost:3000", "https://localhost:3001"}
}
