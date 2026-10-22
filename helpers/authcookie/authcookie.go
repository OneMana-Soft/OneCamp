// Package authcookie writes and reads the sign-in cookies (the access token,
// the refresh token and the device id) and the CSRF cookie. Every sign-in,
// refresh and sign-out writes through it, and every middleware reads through
// it, so all of them carry the same attributes and read the same way.
//
// THE COOKIES ARE THE API HOST'S OWN (21 Oct 2026). They used to be written
// with Domain=FE_DOMAIN so the web app could read the CSRF cookie. On managed
// Cloud FE_DOMAIN is the zone (onemana.dev), because acme.onemana.dev (web)
// and acme-api.onemana.dev (API) share no narrower parent: every workspace's
// session went to every other workspace's hosts, the demo and the storefront,
// and two workspaces in one browser overwrote each other's. Now:
//
//   - No Domain, so only the API host that wrote them gets them. The web app
//     never needed them: its requests carry them because web and API are
//     same-site, and it takes the CSRF token from the answer's header.
//   - __Host- names where cookies are Secure, so no other host on the zone can
//     write or shadow them (a browser refuses a __Host- cookie with a Domain),
//     and so they never share a name with what earlier builds wrote.
//   - SameSite=Lax: the web's fetches are same-site, and OAuth and SAML
//     returns are top-level navigations, which may set Lax cookies and read
//     none. What it refuses is a cross-site page or image carrying the session.
//   - HttpOnly, Path=/, Secure as before.
//
// Browsers still hold, for up to the refresh token's month, the cookies earlier
// builds wrote under the old names on FE_DOMAIN. A server reads its own (signed
// with its key) when nothing newer is there, and the next refresh or sign-in
// moves them: SetSession writes the new cookies and expires the old ones. It
// never reads or expires another workspace's, which arrive under the same names
// on a shared zone: expiring them would sign the person out over there.
package authcookie

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// The cookies' names as written where cookies can't be Secure (plain http in
// development). Name gives the name each is written under.
const (
	Access  = "Authorization"
	Refresh = "RefreshToken"
	Device  = "DeviceId"
	CSRF    = "X-CSRF-Token"
)

// hostPrefix makes a browser refuse the cookie unless it is Secure, on Path=/
// and without a Domain.
const hostPrefix = "__Host-"

// sameSite is every cookie's: the web app and the API are same-site in every
// layout that works (one registrable domain), so Lax goes with its fetches.
const sameSite = http.SameSiteLaxMode

// legacySessionNames are the names earlier builds wrote the session under.
// AuthToken is older still: it is only ever expired.
var legacySessionNames = []string{Access, "AuthToken", Refresh, Device}

// Secure reports whether the cookies are Secure. They are, except where they
// can't be: COOKIE_SECURE=false, or a web app on localhost (development over
// plain http).
func Secure() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("COOKIE_SECURE")), "false") {
		return false
	}
	d := strings.ToLower(strings.TrimSpace(os.Getenv("FE_DOMAIN")))
	if d == "" {
		d = strings.ToLower(strings.TrimSpace(os.Getenv("FRONTEND_DOMAIN")))
	}
	return !strings.Contains(d, "localhost") && !strings.HasPrefix(d, "127.0.0.1")
}

// Name is the name the cookie base is written under: __Host-base where cookies
// are Secure, base where they can't be (a __Host- cookie must be Secure).
func Name(base string) string {
	if Secure() {
		return hostPrefix + base
	}
	return base
}

// Session is what a sign-in or a refresh hands the browser.
type Session struct {
	Access         string
	AccessExpires  time.Time
	Refresh        string
	RefreshExpires time.Time
	// Device keys the refresh token's record; it lives as long as the
	// refresh token.
	Device string
}

// SetSession writes the session's three cookies, on this host alone, and
// expires the copies an earlier build of this server wrote.
func SetSession(w http.ResponseWriter, r *http.Request, s Session) {
	expireOwnLegacy(w, r, Name(Access), Name(Refresh), Name(Device))
	Write(w, Access, s.Access, s.AccessExpires)
	Write(w, Refresh, s.Refresh, s.RefreshExpires)
	Write(w, Device, s.Device, s.RefreshExpires)
}

// ClearSession expires the session's cookies: this host's own, and the copies
// an earlier build of this server wrote. Nothing of another workspace's.
func ClearSession(w http.ResponseWriter, r *http.Request) {
	own := []string{Name(Access), Name(Refresh), Name(Device)}
	for _, name := range own {
		Expire(w, name, "")
	}
	expireOwnLegacy(w, r, own...)
}

// Write sets the cookie base on this host alone (Path=/, HttpOnly, Lax, and
// Secure where cookies are), until expires.
func Write(w http.ResponseWriter, base, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     Name(base),
		Value:    value,
		Path:     "/",
		Expires:  expires,
		Secure:   Secure(),
		HttpOnly: true,
		SameSite: sameSite,
	})
}

// Expire expires the cookie name on domain ("" for this host alone) with
// Max-Age=0. A browser drops a cookie only for a Set-Cookie with its own name,
// Domain and Path, and every cookie here was written on Path=/.
func Expire(w http.ResponseWriter, name, domain string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		Domain:   domain,
		MaxAge:   -1, // Max-Age=0
		Secure:   Secure(),
		HttpOnly: true,
		SameSite: sameSite,
	})
}

// AccessToken is the session's access token. See token.
func AccessToken(r *http.Request) string {
	return token(r, Access, helpers.TokenTypeAccess)
}

// RefreshToken is the session's refresh token. See token.
func RefreshToken(r *http.Request) string {
	return token(r, Refresh, helpers.TokenTypeRefresh)
}

// token is the cookie base's token: this host's own cookie whenever there is
// one, valid or not. Otherwise, while browsers still hold what earlier builds
// wrote, the first cookie under the old name that is a session of kind typ of
// this server's. A browser sends the older cookie of a name first, and on a
// shared zone that may be another workspace's: reading the first of the name
// read that workspace's session, refused it, and signed the person out.
func token(r *http.Request, base, typ string) string {
	if Secure() {
		if c, err := r.Cookie(Name(base)); err == nil && c.Value != "" {
			return c.Value
		}
	}
	for _, c := range r.Cookies() {
		if c.Name != base || c.Value == "" {
			continue
		}
		if _, err := helpers.ParseSessionToken(c.Value, typ); err == nil {
			return c.Value
		}
	}
	return ""
}

// DeviceID is the session's device id: this host's own cookie, or the one an
// earlier build of this server wrote beside its session. A device id is
// opaque, so only the session beside it says whose it is.
func DeviceID(r *http.Request) string {
	if Secure() {
		if c, err := r.Cookie(Name(Device)); err == nil && c.Value != "" {
			return c.Value
		}
		if !legacySessionIsOurs(r) {
			return ""
		}
	}
	if c, err := r.Cookie(Device); err == nil {
		return c.Value
	}
	return ""
}

// LegacyDomain is the Domain earlier builds wrote the cookies on: FE_DOMAIN's
// host, "" when none is configured. It also scopes the readable CSRF cookie
// kept for web builds from before 21 Oct 2026 (middleware/csrfLegacy.go).
func LegacyDomain() string {
	raw := strings.TrimSpace(os.Getenv("FE_DOMAIN"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("FRONTEND_DOMAIN"))
	}
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			raw = u.Host
		}
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	} else if i := strings.Index(raw, ":"); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

// legacySessionIsOurs reports whether the request carries a session under the
// old names that this server signed (expired or not): its own, from before the
// cookies moved, rather than another workspace's.
func legacySessionIsOurs(r *http.Request) bool {
	for _, c := range r.Cookies() {
		if (c.Name == Access || c.Name == Refresh) && helpers.SessionTokenSignedHere(c.Value) {
			return true
		}
	}
	return false
}

// expireOwnLegacy expires the session cookies an earlier build of this server
// wrote under the old names, among those the request carries: on FE_DOMAIN
// when the session in them is this server's own, and host-only always (only
// this host can have written a host-only cookie for itself; where none exists
// the browser ignores it). A browser that didn't send one doesn't have it:
// they were all written on Path=/. skip are names this answer writes or
// expires itself, which in development are the old names.
func expireOwnLegacy(w http.ResponseWriter, r *http.Request, skip ...string) {
	received := map[string]bool{}
	for _, c := range r.Cookies() {
		received[c.Name] = true
	}
	skipped := map[string]bool{}
	for _, s := range skip {
		skipped[s] = true
	}
	domain := ""
	if legacySessionIsOurs(r) {
		domain = LegacyDomain()
	}
	for _, name := range legacySessionNames {
		if !received[name] {
			continue
		}
		if !skipped[name] {
			Expire(w, name, "")
		}
		if domain != "" {
			Expire(w, name, domain)
		}
	}
}
