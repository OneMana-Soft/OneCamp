package middleware

// Response security headers for the API.
//
// WHY HERE RATHER THAN THE PROXY. The frontend's headers were deferred to "the
// upstream proxy" and never set there; a live check found no Content-Security-
// Policy on the app and no Strict-Transport-Security on the API. A header the
// application sets travels with the application: it survives a customer running
// a different proxy, terminating TLS somewhere else, or reaching the API
// directly. The proxy may still add its own, and duplicates are harmless because
// each of these is a fixed value rather than a list a second layer could weaken.
//
// These are the API-shaped subset. There is no CSP here because a JSON API has
// no document to constrain, and no Permissions-Policy because it renders nothing.

import (
	"net/http"
	"strings"
)

// hstsMaxAge is two years in seconds, the value the frontend already advertises.
// Kept identical so a browser does not see the two halves of the same product
// disagree about how long to remember the requirement.
const hstsMaxAge = "max-age=63072000"

// requestIsHTTPS reports whether the ORIGINAL request reached the edge over TLS.
//
// r.TLS is nil here even on an https request, because TLS is terminated at the
// proxy and this process is spoken to over plain http on the internal network.
// Sending HSTS on a genuinely plaintext deployment would be a promise the
// operator cannot keep: a browser that caches it can no longer reach an http-only
// install at all, and that is not recoverable from the server side.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// Set by Traefik and every other common reverse proxy. A comma-separated
	// list appears when more than one proxy is in front; the first entry is the
	// original client-facing scheme.
	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// SecurityHeaders sets the response headers that are correct for every API
// response. It never overwrites a header a handler has already set deliberately.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()

		// A JSON API's content type must never be re-interpreted. This is what
		// stops an uploaded file served through the API being sniffed as HTML
		// and executed in the user's origin.
		setIfAbsent(h, "X-Content-Type-Options", "nosniff")

		// DENY rather than SAMEORIGIN: nothing should ever frame the API. The
		// frontend uses SAMEORIGIN because it frames its own document previews.
		setIfAbsent(h, "X-Frame-Options", "DENY")

		// API responses carry ids in their paths. Do not leak them to anywhere
		// the user navigates next.
		setIfAbsent(h, "Referrer-Policy", "no-referrer")

		// Keep API responses out of other origins' documents.
		setIfAbsent(h, "Cross-Origin-Resource-Policy", "same-site")

		if requestIsHTTPS(r) {
			setIfAbsent(h, "Strict-Transport-Security", hstsMaxAge)
		}

		next.ServeHTTP(w, r)
	})
}

// setIfAbsent leaves a value a handler already chose. A handler that sets one of
// these has a reason (a download that must not be sniffed differently, say), and
// a blanket middleware should not silently overrule it.
func setIfAbsent(h http.Header, key, value string) {
	if h.Get(key) == "" {
		h.Set(key, value)
	}
}
