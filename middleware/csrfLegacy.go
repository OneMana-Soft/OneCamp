package middleware

// Web builds from before 21 Oct 2026 read the CSRF token from document.cookie:
// a cookie named X-CSRF-Token on FE_DOMAIN, which the API wrote on every
// answer. A tab left open across an update runs one, and so does a self-hosted
// web app built apart from the server, until it is rebuilt.
//
// So for a request that doesn't say it comes from a later web app (those send
// X-Requested-With: XMLHttpRequest, a header every deployed API's CORS has
// allowed since February 2026), that readable cookie is still written, and a
// header matching it still passes. It keeps the value the browser already has:
// on a shared zone the other workspaces' servers write the same cookie, and
// their older tabs read it too. A later web app never causes it, so a browser
// that runs only later ones holds no cookie on the zone at all, and later web
// apps are held to the API host's own token.
//
// CSRF_LEGACY_COOKIE=off stops it, and expires the cookie when a request
// brings it. The next release turns it off by default; the one after removes
// this file.

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/authcookie"
)

const (
	legacyCSRFCookieName = "X-CSRF-Token"
	// webClientHeader and webClientValue are how a web app that reads the
	// token from the answer's header says so.
	webClientHeader = "X-Requested-With"
	webClientValue  = "XMLHttpRequest"
)

// currentWebClient reports whether the request comes from a web app that reads
// the CSRF token from the answer's header.
func currentWebClient(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get(webClientHeader)), webClientValue)
}

func legacyCSRFCookieOn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CSRF_LEGACY_COOKIE"))) {
	case "off", "false", "0", "no":
		return false
	}
	return true
}

// legacyCSRFValues are the values of the readable cookies the request carries.
func legacyCSRFValues(r *http.Request) []string {
	var out []string
	for _, c := range r.Cookies() {
		if c.Name == legacyCSRFCookieName && c.Value != "" {
			out = append(out, c.Value)
		}
	}
	return out
}

// keepLegacyCSRFCookie writes the readable cookie for an older web app, or,
// with CSRF_LEGACY_COOKIE=off, expires it when the request brings one.
func keepLegacyCSRFCookie(w http.ResponseWriter, r *http.Request) {
	domain := authcookie.LegacyDomain()
	if domain == "" {
		return
	}
	existing := legacyCSRFValues(r)
	if !legacyCSRFCookieOn() {
		if len(existing) > 0 {
			authcookie.Expire(w, legacyCSRFCookieName, domain)
		}
		return
	}
	if currentWebClient(r) {
		return
	}
	value := ""
	if len(existing) > 0 {
		value = existing[0]
	} else {
		minted, err := generateCSRFToken()
		if err != nil {
			helpers.LogErrorWithContext(r.Context(), "CSRF legacy token mint failed: %+v", err)
			return
		}
		value = minted
	}
	http.SetCookie(w, &http.Cookie{
		Name:    legacyCSRFCookieName,
		Value:   value,
		Domain:  domain,
		Path:    "/",
		Expires: time.Now().Add(csrfCookieTTL),
		Secure:  authcookie.Secure(),
		// Readable: an older web app reads it from document.cookie.
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
}

// legacyCSRFMatches reports whether an older web app's header matches a
// readable cookie the request carries. Never for a later web app.
func legacyCSRFMatches(r *http.Request, header string) bool {
	if currentWebClient(r) || !legacyCSRFCookieOn() {
		return false
	}
	for _, v := range legacyCSRFValues(r) {
		if subtle.ConstantTimeCompare([]byte(header), []byte(v)) == 1 {
			return true
		}
	}
	return false
}
