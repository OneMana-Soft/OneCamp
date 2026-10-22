package middleware

// The CSRF cookie is the API host's own. It used to sit on FE_DOMAIN so the web
// app could read it from document.cookie; on Cloud FE_DOMAIN is the zone
// (onemana.dev), so every workspace's token went to every other workspace's
// hosts, the demo and the storefront, and any page on the zone could read it or
// write one of its own. Now the cookie is host-only, __Host-, HttpOnly and Lax,
// and the web app takes the token from the answer's X-CSRF-Token header.
//
// A web build from before this release still reads document.cookie. For a
// request that doesn't say it is this release's web (X-Requested-With:
// XMLHttpRequest), the readable cookie on FE_DOMAIN is still written, and a
// header matching it still passes, until CSRF_LEGACY_COOKIE=off.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	webOrigin   = "https://acme.onemana.dev"
	hostCSRF    = "__Host-X-CSRF-Token"
	legacyCSRF  = "X-CSRF-Token"
	markHeader  = "X-Requested-With"
	markCurrent = "XMLHttpRequest"
)

// csrfEnv is a managed Cloud workspace: web acme.onemana.dev, API
// acme-api.onemana.dev, FE_DOMAIN the zone.
func csrfEnv(t *testing.T) {
	t.Helper()
	t.Setenv("FE_DOMAIN", "onemana.dev")
	t.Setenv("FRONTEND_DOMAIN", "")
	t.Setenv("FE_HOST_DOMAIN", "acme.onemana.dev")
	t.Setenv("BACKEND_DOMAIN", "acme-api.onemana.dev")
	t.Setenv("COOKIE_SECURE", "")
	t.Setenv("CSRF_DISABLED", "")
	t.Setenv("CSRF_ALLOWED_ORIGINS", "")
	t.Setenv("CSRF_LEGACY_COOKIE", "")
}

// csrfRequest is a request from the web app at origin; current says it is this
// release's web.
func csrfRequest(method, origin string, current bool, cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest(method, "https://acme-api.onemana.dev/channel/list", nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if current {
		r.Header.Set(markHeader, markCurrent)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

func serveCSRF(r *http.Request) (*httptest.ResponseRecorder, bool) {
	reached := false
	h := CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec, reached
}

func setCookies(rec *httptest.ResponseRecorder, name string) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// cookieNames names what an answer set, without values.
func cookieNames(rec *httptest.ResponseRecorder) []string {
	var out []string
	for _, c := range rec.Result().Cookies() {
		out = append(out, c.Name+" Domain="+c.Domain)
	}
	return out
}

// withToken gives a request the same token in the header and in both cookies,
// the host's own and the old one, so only the origin decides: old and new code
// both accept the token itself.
func withToken(r *http.Request, tok string) *http.Request {
	r.AddCookie(&http.Cookie{Name: hostCSRF, Value: tok})
	r.AddCookie(&http.Cookie{Name: legacyCSRF, Value: tok})
	r.Header.Set("X-CSRF-Token", tok)
	return r
}

func zoneCookies(rec *httptest.ResponseRecorder) []string {
	var out []string
	for _, c := range rec.Result().Cookies() {
		if c.Domain != "" {
			out = append(out, c.Name+" on "+c.Domain)
		}
	}
	return out
}

func TestTheCSRFCookieIsTheAPIHostsOwn(t *testing.T) {
	csrfEnv(t)
	rec, reached := serveCSRF(csrfRequest(http.MethodGet, webOrigin, true))
	if !reached {
		t.Fatal("a read was stopped")
	}
	got := setCookies(rec, hostCSRF)
	if len(got) != 1 {
		t.Fatalf("Set-Cookie %s: %d of them, want 1 (all: %v)", hostCSRF, len(got), cookieNames(rec))
	}
	c := got[0]
	if c.Domain != "" {
		t.Errorf("Domain=%q: the token went to every host under it", c.Domain)
	}
	if c.Path != "/" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("Path=%q Secure=%v HttpOnly=%v SameSite=%v, want / true true Lax", c.Path, c.Secure, c.HttpOnly, c.SameSite)
	}
	if left := time.Until(c.Expires); left < 23*time.Hour || left > 25*time.Hour {
		t.Errorf("lives %v, want a day", left)
	}
	if z := zoneCookies(rec); len(z) != 0 {
		t.Errorf("this release's web got cookies on the zone: %v", z)
	}
	if h := rec.Header().Get("X-CSRF-Token"); h == "" || h != c.Value {
		t.Error("the answer doesn't carry the token in force as X-CSRF-Token")
	}
}

func TestTheTokenStaysWhileItsCookieLasts(t *testing.T) {
	csrfEnv(t)
	rec, _ := serveCSRF(csrfRequest(http.MethodGet, webOrigin, true, &http.Cookie{Name: hostCSRF, Value: "steady-token"}))
	if rec.Header().Get("X-CSRF-Token") != "steady-token" {
		t.Error("the token changed while its cookie lived")
	}
	if got := setCookies(rec, hostCSRF); len(got) != 1 || got[0].Value != "steady-token" {
		t.Error("the cookie wasn't written again with the same token")
	}
}

func TestAWriteFromTheWebPassesWithTheTokenItWasGiven(t *testing.T) {
	csrfEnv(t)
	r := csrfRequest(http.MethodPost, webOrigin, true, &http.Cookie{Name: hostCSRF, Value: "the-token"})
	r.Header.Set("X-CSRF-Token", "the-token")
	if rec, reached := serveCSRF(r); !reached {
		t.Fatalf("refused (%d %s)", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

func TestAWriteFromTheWebIsNotPassedByAZoneCookie(t *testing.T) {
	csrfEnv(t)
	// Any page on the zone can read and write the old cookie, so for this
	// release's web only the API host's own token counts.
	r := csrfRequest(http.MethodPost, webOrigin, true,
		&http.Cookie{Name: legacyCSRF, Value: "zone-value"},
		&http.Cookie{Name: hostCSRF, Value: "the-token"})
	r.Header.Set("X-CSRF-Token", "zone-value")
	rec, reached := serveCSRF(r)
	if reached || rec.Code != http.StatusForbidden {
		t.Fatalf("passed on the zone cookie (%d)", rec.Code)
	}
}

func TestARefusedWriteSaysTheTokenToRetryWith(t *testing.T) {
	csrfEnv(t)
	// The cookie ran out between two requests: the answer names the new token,
	// and the web retries once with it.
	r := csrfRequest(http.MethodPost, webOrigin, true)
	r.Header.Set("X-CSRF-Token", "token-from-yesterday")
	rec, reached := serveCSRF(r)
	if reached || rec.Code != http.StatusForbidden {
		t.Fatalf("a token nobody holds passed (%d)", rec.Code)
	}
	fresh := rec.Header().Get("X-CSRF-Token")
	if fresh == "" || fresh == "token-from-yesterday" {
		t.Errorf("the refusal doesn't name a new token: %q", fresh)
	}
	if got := setCookies(rec, hostCSRF); len(got) != 1 || got[0].Value != fresh {
		t.Error("the new token's cookie isn't in the refusal")
	}
}

func TestAWebBuildFromBeforeThisReleaseStillWrites(t *testing.T) {
	csrfEnv(t)
	// No X-Requested-With: the web reads the token from document.cookie, so
	// the readable cookie on FE_DOMAIN is still written for it...
	rec, _ := serveCSRF(csrfRequest(http.MethodGet, webOrigin, false))
	legacy := setCookies(rec, legacyCSRF)
	if len(legacy) != 1 {
		t.Fatalf("Set-Cookie %s: %d, want 1", legacyCSRF, len(legacy))
	}
	l := legacy[0]
	if l.Domain != "onemana.dev" || l.HttpOnly || !l.Secure || l.Path != "/" || l.Value == "" {
		t.Errorf("Domain=%q HttpOnly=%v Secure=%v Path=%q: an older web can't read it", l.Domain, l.HttpOnly, l.Secure, l.Path)
	}
	// ...and is never the API host's own token, which stays secret.
	if own := setCookies(rec, hostCSRF); len(own) != 1 || own[0].Value == l.Value {
		t.Error("the zone cookie carries the host's own token")
	}

	// It sends that value back, and the write passes.
	w := csrfRequest(http.MethodPost, webOrigin, false, &http.Cookie{Name: legacyCSRF, Value: l.Value})
	w.Header.Set("X-CSRF-Token", l.Value)
	if rec, reached := serveCSRF(w); !reached {
		t.Fatalf("an older web's write was refused (%d)", rec.Code)
	}
}

func TestTheOlderWebKeepsTheValueItAlreadyHas(t *testing.T) {
	csrfEnv(t)
	// Other workspaces' servers write the same zone cookie; rewriting it with a
	// new value would fail their older tabs' next write.
	rec, _ := serveCSRF(csrfRequest(http.MethodGet, webOrigin, false, &http.Cookie{Name: legacyCSRF, Value: "already-there"}))
	if got := setCookies(rec, legacyCSRF); len(got) != 1 || got[0].Value != "already-there" {
		t.Errorf("the zone cookie's value changed: %v", got)
	}
}

func TestCSRFLegacyCookieOffStopsTheZoneCookie(t *testing.T) {
	csrfEnv(t)
	t.Setenv("CSRF_LEGACY_COOKIE", "off")
	rec, _ := serveCSRF(csrfRequest(http.MethodGet, webOrigin, false, &http.Cookie{Name: legacyCSRF, Value: "zone-value"}))
	for _, c := range setCookies(rec, legacyCSRF) {
		if c.MaxAge >= 0 {
			t.Errorf("the zone cookie was written (%q on %q), not expired", c.Value, c.Domain)
		}
	}
	w := csrfRequest(http.MethodPost, webOrigin, false, &http.Cookie{Name: legacyCSRF, Value: "zone-value"})
	w.Header.Set("X-CSRF-Token", "zone-value")
	if rec, reached := serveCSRF(w); reached || rec.Code != http.StatusForbidden {
		t.Fatalf("the zone cookie still passes a write (%d)", rec.Code)
	}
}

func TestTheRefererMustBeTheWebsOwnOrigin(t *testing.T) {
	csrfEnv(t)
	write := func(referer string) bool {
		r := withToken(csrfRequest(http.MethodPost, "", true), "the-token")
		r.Header.Set("Referer", referer)
		_, reached := serveCSRF(r)
		return reached
	}
	// A prefix match let any host that begins with the web's name in.
	if write("https://acme.onemana.dev.evil.example/page") {
		t.Error("a Referer on another host that starts with the web's origin passed")
	}
	if write("https://acme.onemana.dev:8443/page") {
		t.Error("a Referer on another port passed")
	}
	if !write("https://acme.onemana.dev/app/home") {
		t.Error("the web's own Referer was refused")
	}
}

func TestAnUntrustedOriginIsNotRescuedByItsReferer(t *testing.T) {
	csrfEnv(t)
	r := withToken(csrfRequest(http.MethodPost, "https://evil.example", true), "the-token")
	r.Header.Set("Referer", "https://acme.onemana.dev/app/home")
	if _, reached := serveCSRF(r); reached {
		t.Fatal("an untrusted Origin passed on a trusted Referer")
	}
}

func TestPlainHTTPOriginsAreTrustedOnlyInDevelopment(t *testing.T) {
	csrfEnv(t)
	write := func() bool {
		r := withToken(csrfRequest(http.MethodPost, "http://acme.onemana.dev", true), "the-token")
		_, reached := serveCSRF(r)
		return reached
	}
	if write() {
		t.Error("a plain-http origin passed on an https install")
	}
	t.Setenv("COOKIE_SECURE", "false")
	if !write() {
		t.Error("a plain-http origin was refused where cookies are insecure (development)")
	}
}

func TestInDevelopmentTheCookieHasNoPrefixAndIsNotSecure(t *testing.T) {
	csrfEnv(t)
	t.Setenv("COOKIE_SECURE", "false")
	t.Setenv("FE_DOMAIN", "localhost")
	t.Setenv("FE_HOST_DOMAIN", "localhost:3000")
	rec, _ := serveCSRF(csrfRequest(http.MethodGet, "http://localhost:3000", true))
	got := setCookies(rec, legacyCSRF)
	if len(got) != 1 {
		t.Fatalf("Set-Cookie %s: %d, want 1 (%v)", legacyCSRF, len(got), cookieNames(rec))
	}
	c := got[0]
	if c.Domain != "" || c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("Domain=%q Secure=%v HttpOnly=%v SameSite=%v, want host-only, insecure, HttpOnly, Lax", c.Domain, c.Secure, c.HttpOnly, c.SameSite)
	}
	if len(setCookies(rec, hostCSRF)) != 0 {
		t.Error("a __Host- cookie on plain http, which browsers refuse")
	}
}

func TestCSRFDisabledStillHandsOutTheToken(t *testing.T) {
	csrfEnv(t)
	t.Setenv("CSRF_DISABLED", "true")
	rec, reached := serveCSRF(csrfRequest(http.MethodPost, webOrigin, true))
	if !reached {
		t.Fatal("CSRF_DISABLED still refused a write")
	}
	if rec.Header().Get("X-CSRF-Token") == "" || len(setCookies(rec, hostCSRF)) != 1 {
		t.Error("no token handed out while disabled, so the web would fail once enabled")
	}
}
