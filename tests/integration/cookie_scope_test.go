//go:build integration
// +build integration

package integration_test

// Sign-in cookies stay on the workspace's own API host (COOKIE-SCOPE, 21 Oct
// 2026). Before, they were written on FE_DOMAIN, which on managed Cloud is the
// zone (onemana.dev): every workspace's session went to every other
// workspace's hosts, and two workspaces in one browser overwrote each other.
//
// This drives the real router, against real Postgres, Redis and Dgraph, the
// way a browser on https://acme.onemana.dev does: a cookie jar that follows
// the Domain rules, so what the browser would send to another workspace's
// host (beta-api.onemana.dev) or the storefront (onemana.dev) is read from it.
//
// Run: go test -tags=integration ./tests/integration/ -run TestSignInCookiesStayOnTheWorkspacesOwnAPIHost -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/publicsuffix"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

const (
	cookieScopeOurKey   = "acme-cookie-scope-integration-secret-0123456789"
	cookieScopeTheirKey = "beta-cookie-scope-integration-secret-0123456789"
	acmeWeb             = "https://acme.onemana.dev"
	acmeAPI             = "acme-api.onemana.dev"
)

// cookieBrowser is a browser on acme.onemana.dev, as far as cookies go.
type cookieBrowser struct {
	t      *testing.T
	jar    *cookiejar.Jar
	routes http.Handler
}

type cookieAnswer struct {
	code    int
	header  http.Header
	cookies []*http.Cookie
}

// call sends a request to the acme API from the acme web app. current says it
// is this release's web app (X-Requested-With); csrf is the token it echoes.
func (b *cookieBrowser) call(method, path string, body any, current bool, csrf string, origin string) cookieAnswer {
	b.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, "https://"+acmeAPI+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if current {
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	for _, c := range b.jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	b.routes.ServeHTTP(rec, req)
	res := rec.Result()
	b.jar.SetCookies(req.URL, res.Cookies())
	return cookieAnswer{code: res.StatusCode, header: res.Header, cookies: res.Cookies()}
}

// sentTo names the cookies the browser would send to host.
func (b *cookieBrowser) sentTo(host string) map[string]bool {
	out := map[string]bool{}
	for _, c := range b.jar.Cookies(&url.URL{Scheme: "https", Host: host, Path: "/"}) {
		out[c.Name] = true
	}
	return out
}

// plant puts cookies in the jar as an earlier build, or another workspace,
// wrote them: on the zone.
func (b *cookieBrowser) plant(names map[string]string) {
	var cs []*http.Cookie
	for n, v := range names {
		cs = append(cs, &http.Cookie{Name: n, Value: v, Domain: "onemana.dev", Path: "/", Secure: true, HttpOnly: true, Expires: time.Now().Add(time.Hour)})
	}
	b.jar.SetCookies(&url.URL{Scheme: "https", Host: acmeAPI, Path: "/"}, cs)
}

func TestSignInCookiesStayOnTheWorkspacesOwnAPIHost(t *testing.T) {
	t.Setenv("JWT_SECRET", cookieScopeOurKey)
	t.Setenv("FE_DOMAIN", "onemana.dev")
	t.Setenv("FRONTEND_DOMAIN", "")
	t.Setenv("FE_HOST_DOMAIN", "acme.onemana.dev")
	t.Setenv("BACKEND_DOMAIN", acmeAPI)
	t.Setenv("COOKIE_SECURE", "")
	t.Setenv("CSRF_DISABLED", "")
	t.Setenv("CSRF_ALLOWED_ORIGINS", "")
	t.Setenv("CSRF_LEGACY_COOKIE", "")
	t.Setenv("ALLOWED_USERS", "")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	integration.SetupDgraph(t)

	const email, password = "sam@acme.test", "a long enough password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	hashed := string(hash)
	userID, _, err := userBusiness.CreateUserWithMethod(ctx, email, "Sam Rivera", &hashed, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatalf("create the member: %v", err)
	}

	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		t.Fatal(err)
	}
	b := &cookieBrowser{t: t, jar: jar, routes: router.Routes()}
	session := []string{"__Host-Authorization", "__Host-RefreshToken", "__Host-DeviceId"}
	old := []string{"Authorization", "RefreshToken", "DeviceId"}
	signIn := func() {
		t.Helper()
		a := b.call(http.MethodPost, "/auth/login", map[string]string{"email": email, "password": password}, true, "", acmeWeb)
		if a.code != http.StatusOK {
			t.Fatalf("sign-in answered %d", a.code)
		}
		set := map[string]*http.Cookie{}
		for _, c := range a.cookies {
			if c.Domain != "" {
				t.Errorf("sign-in wrote %s on %s", c.Name, c.Domain)
			}
			set[c.Name] = c
		}
		for _, n := range session {
			c := set[n]
			if c == nil {
				t.Fatalf("sign-in didn't write %s", n)
			}
			if c.Path != "/" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
				t.Errorf("%s: Path=%q Secure=%v HttpOnly=%v SameSite=%v", n, c.Path, c.Secure, c.HttpOnly, c.SameSite)
			}
		}
	}
	profile := func(current bool) cookieAnswer {
		t.Helper()
		return b.call(http.MethodGet, "/user/profile", nil, current, "", acmeWeb)
	}
	theme := func(current bool, token, origin string) int {
		t.Helper()
		return b.call(http.MethodPost, "/updateUserTheme", map[string]string{"user_theme_mode": "dark"}, current, token, origin).code
	}
	noneOf := func(host string, names []string) {
		t.Helper()
		sent := b.sentTo(host)
		for _, n := range names {
			if sent[n] {
				t.Errorf("the browser sends %s to %s", n, host)
			}
		}
	}

	// 1. Signing in writes the session on the API host alone.
	signIn()
	for _, host := range []string{"beta-api.onemana.dev", "beta.onemana.dev", "onemana.dev", "acme.onemana.dev", "acme-minio.onemana.dev"} {
		noneOf(host, append(append([]string{}, session...), old...))
	}

	// 2. A read answers with the CSRF token, readable through CORS, and writes
	// no cookie anywhere but here.
	read := profile(true)
	if read.code != http.StatusOK {
		t.Fatalf("the profile answered %d", read.code)
	}
	token := read.header.Get("X-CSRF-Token")
	if token == "" {
		t.Fatal("the answer doesn't carry the CSRF token")
	}
	if !strings.Contains(strings.ToLower(read.header.Get("Access-Control-Expose-Headers")), "x-csrf-token") {
		t.Error("the web app can't read the token's header")
	}
	if got := len(b.sentTo("beta-api.onemana.dev")); got != 0 {
		t.Errorf("the browser sends %d cookies to another workspace's API", got)
	}
	if !b.sentTo(acmeAPI)["__Host-X-CSRF-Token"] {
		t.Error("the CSRF cookie isn't on the API host")
	}
	if a := b.call(http.MethodGet, "/auth/csrf", nil, true, "", acmeWeb); a.code != http.StatusNoContent || a.header.Get("X-CSRF-Token") != token {
		t.Errorf("GET /auth/csrf answered %d, same token=%v", a.code, a.header.Get("X-CSRF-Token") == token)
	}

	// 3. Writes: the header token passes; none, or the wrong origin, doesn't.
	if code := theme(true, token, acmeWeb); code != http.StatusOK {
		t.Errorf("a write with the token answered %d", code)
	}
	if code := theme(true, "", acmeWeb); code != http.StatusForbidden {
		t.Errorf("a write without the token answered %d", code)
	}
	if code := theme(true, token, "https://acme.onemana.dev.evil.example"); code != http.StatusForbidden {
		t.Errorf("a write from a look-alike origin answered %d", code)
	}

	// 4. Refreshing keeps the session on the API host.
	if a := b.call(http.MethodGet, "/refreshToken", nil, true, "", acmeWeb); a.code != http.StatusOK {
		t.Fatalf("refresh answered %d", a.code)
	}
	if read := profile(true); read.code != http.StatusOK {
		t.Errorf("after a refresh the profile answered %d", read.code)
	}
	noneOf("beta-api.onemana.dev", append(append([]string{}, session...), old...))

	// 5. Signing out ends it.
	if a := b.call(http.MethodPost, "/logout", nil, true, "", acmeWeb); a.code != http.StatusOK {
		t.Fatalf("sign-out answered %d", a.code)
	}
	if read := profile(true); read.code != http.StatusUnauthorized {
		t.Errorf("after signing out the profile answered %d", read.code)
	}

	// 6. A browser holding this server's session as an earlier build wrote it
	// (on the zone) stays signed in, and its next refresh moves it here.
	access, refresh, err := userBusiness.StartSession(ctx, userID.String(), "device-from-before", time.Now().Add(6*time.Minute).Unix(), time.Now().Add(24*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	b.plant(map[string]string{"Authorization": access, "RefreshToken": refresh, "DeviceId": "device-from-before"})
	if read := profile(true); read.code != http.StatusOK {
		t.Fatalf("with this server's own old cookies the profile answered %d", read.code)
	}
	if a := b.call(http.MethodGet, "/refreshToken", nil, true, "", acmeWeb); a.code != http.StatusOK {
		t.Fatalf("refreshing the old session answered %d", a.code)
	}
	noneOf("beta-api.onemana.dev", old)
	noneOf("onemana.dev", old)
	for _, n := range session {
		if !b.sentTo(acmeAPI)[n] {
			t.Errorf("after the refresh %s isn't on the API host", n)
		}
	}
	if read := profile(true); read.code != http.StatusOK {
		t.Errorf("after moving the old session the profile answered %d", read.code)
	}
	if a := b.call(http.MethodPost, "/logout", nil, true, "", acmeWeb); a.code != http.StatusOK {
		t.Fatalf("sign-out answered %d", a.code)
	}

	// 7. Another workspace's cookies on the zone (beta, not yet updated) are
	// neither read nor touched: signing in and out here leaves beta signed in.
	t.Setenv("JWT_SECRET", cookieScopeTheirKey)
	theirAccess, err := helpers.SignSessionToken(uuid.NewString(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	theirRefresh, err := helpers.SignSessionToken(uuid.NewString(), helpers.TokenTypeRefresh, time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("JWT_SECRET", cookieScopeOurKey)
	b.plant(map[string]string{"Authorization": theirAccess, "RefreshToken": theirRefresh, "DeviceId": "their-device"})
	signIn()
	if read := profile(true); read.code != http.StatusOK {
		t.Fatalf("with beta's cookies beside ours the profile answered %d", read.code)
	}
	if a := b.call(http.MethodGet, "/refreshToken", nil, true, "", acmeWeb); a.code != http.StatusOK {
		t.Fatalf("refresh with beta's cookies beside ours answered %d", a.code)
	}
	if a := b.call(http.MethodPost, "/logout", nil, true, "", acmeWeb); a.code != http.StatusOK {
		t.Fatalf("sign-out answered %d", a.code)
	}
	for _, n := range old {
		if !b.sentTo("beta-api.onemana.dev")[n] {
			t.Errorf("beta's %s is gone: signing in and out here signed the person out of beta", n)
		}
	}

	// 8. A web build from before this release (no X-Requested-With) reads the
	// token from a cookie it can see, on FE_DOMAIN, and its writes pass.
	signIn()
	if read := profile(false); read.code != http.StatusOK {
		t.Fatalf("an older web's read answered %d", read.code)
	}
	var visible string
	for _, c := range b.jar.Cookies(&url.URL{Scheme: "https", Host: "acme.onemana.dev", Path: "/"}) {
		if c.Name == "X-CSRF-Token" {
			visible = c.Value
		}
	}
	if visible == "" {
		t.Fatal("an older web has no CSRF cookie it can read")
	}
	if code := theme(false, visible, acmeWeb); code != http.StatusOK {
		t.Errorf("an older web's write answered %d", code)
	}
	// This release's web is held to the host's own token.
	if code := theme(true, visible, acmeWeb); code != http.StatusForbidden {
		t.Errorf("this release's web passed on the zone cookie: %d", code)
	}
	t.Setenv("CSRF_LEGACY_COOKIE", "off")
	if code := theme(false, visible, acmeWeb); code != http.StatusForbidden {
		t.Errorf("with CSRF_LEGACY_COOKIE=off an older web's write answered %d", code)
	}
}
