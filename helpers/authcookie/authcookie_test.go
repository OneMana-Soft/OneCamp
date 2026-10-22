package authcookie

// Sign-in cookies are the API host's own: host-only, __Host-, HttpOnly, Lax.
//
// They used to be written on FE_DOMAIN, which on managed Cloud is the zone
// (onemana.dev): every workspace's session went to every other workspace's
// hosts, the demo and the storefront, and two workspaces in one browser
// overwrote each other. Browsers still hold those cookies, under the old names,
// for a while; a server reads its own (the ones signed with its key) until the
// next refresh or sign-in moves them, and never touches another workspace's.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
)

const (
	ourKey     = "acme-workspace-secret-0123456789abcdef"
	theirKey   = "beta-workspace-secret-0123456789abcdef"
	zone       = "onemana.dev"
	hostAccess = "__Host-Authorization"
	hostRefr   = "__Host-RefreshToken"
	hostDevice = "__Host-DeviceId"
)

// cloud is a managed Cloud workspace: FE_DOMAIN is the zone.
func cloud(t *testing.T) {
	t.Helper()
	t.Setenv("JWT_SECRET", ourKey)
	t.Setenv("FE_DOMAIN", zone)
	t.Setenv("FRONTEND_DOMAIN", "")
	t.Setenv("FE_HOST_DOMAIN", "acme.onemana.dev")
	t.Setenv("BACKEND_DOMAIN", "acme-api.onemana.dev")
	t.Setenv("COOKIE_SECURE", "")
}

// signed signs a session token of kind typ with key.
func signed(t *testing.T, key, typ string, lives time.Duration) string {
	t.Helper()
	old := os.Getenv("JWT_SECRET")
	t.Setenv("JWT_SECRET", key)
	s, err := helpers.SignSessionToken(uuid.NewString(), typ, time.Now().Add(lives).Unix())
	t.Setenv("JWT_SECRET", old)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func request(cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "https://acme-api.onemana.dev/user/profile", nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

func written(rec *httptest.ResponseRecorder) map[string][]*http.Cookie {
	out := map[string][]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		out[c.Name] = append(out[c.Name], c)
	}
	return out
}

func session() Session {
	now := time.Now()
	return Session{
		Access: "access-jwt", AccessExpires: now.Add(5 * time.Minute),
		Refresh: "refresh-jwt", RefreshExpires: now.Add(30 * 24 * time.Hour),
		Device: "device-1",
	}
}

func TestNamesCarryTheHostPrefixWhereCookiesAreSecure(t *testing.T) {
	cloud(t)
	if !Secure() {
		t.Fatal("an https install's cookies aren't Secure")
	}
	for base, want := range map[string]string{Access: hostAccess, Refresh: hostRefr, Device: hostDevice, CSRF: "__Host-X-CSRF-Token"} {
		if got := Name(base); got != want {
			t.Errorf("Name(%q) = %q, want %q", base, got, want)
		}
	}
	// Plain http (development) can't hold a __Host- cookie at all.
	for name, env := range map[string][2]string{
		"COOKIE_SECURE=false": {"COOKIE_SECURE", "false"},
		"a localhost web":     {"FE_DOMAIN", "localhost"},
		"a loopback web":      {"FE_DOMAIN", "127.0.0.1:3000"},
	} {
		t.Run(name, func(t *testing.T) {
			cloud(t)
			t.Setenv(env[0], env[1])
			if Secure() || Name(Access) != Access {
				t.Errorf("Secure=%v Name=%q, want false and the plain name", Secure(), Name(Access))
			}
		})
	}
}

func TestSetSessionWritesThreeHostOnlyCookies(t *testing.T) {
	cloud(t)
	s := session()
	rec := httptest.NewRecorder()
	SetSession(rec, request(), s)
	got := written(rec)
	for name, want := range map[string]struct {
		value   string
		expires time.Time
	}{
		hostAccess: {s.Access, s.AccessExpires},
		hostRefr:   {s.Refresh, s.RefreshExpires},
		hostDevice: {s.Device, s.RefreshExpires},
	} {
		cs := got[name]
		if len(cs) != 1 {
			t.Errorf("%s: %d Set-Cookie, want 1", name, len(cs))
			continue
		}
		c := cs[0]
		if c.Value != want.value {
			t.Errorf("%s carries the wrong value", name)
		}
		if c.Domain != "" {
			t.Errorf("%s: Domain=%q, so it goes to every host under it", name, c.Domain)
		}
		if c.Path != "/" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
			t.Errorf("%s: Path=%q Secure=%v HttpOnly=%v SameSite=%v, want / true true Lax", name, c.Path, c.Secure, c.HttpOnly, c.SameSite)
		}
		if d := c.Expires.Sub(want.expires); d > time.Second || d < -time.Second {
			t.Errorf("%s runs out at %v, want %v", name, c.Expires, want.expires)
		}
	}
	for name, cs := range got {
		for _, c := range cs {
			if c.Domain != "" {
				t.Errorf("a browser with no old cookies got %s on %s", name, c.Domain)
			}
		}
	}
}

func TestSetSessionMovesThisServersOwnOldCookies(t *testing.T) {
	cloud(t)
	rec := httptest.NewRecorder()
	SetSession(rec, request(
		&http.Cookie{Name: Access, Value: signed(t, ourKey, helpers.TokenTypeAccess, 5*time.Minute)},
		&http.Cookie{Name: Refresh, Value: signed(t, ourKey, helpers.TokenTypeRefresh, 24*time.Hour)},
		&http.Cookie{Name: Device, Value: "device-0"},
	), session())
	got := written(rec)
	for _, name := range []string{Access, Refresh, Device} {
		var onZone, hostOnly bool
		for _, c := range got[name] {
			if c.MaxAge >= 0 || c.Value != "" || c.Path != "/" {
				t.Errorf("%s on %q is written (MaxAge=%d, Path=%q), not expired", name, c.Domain, c.MaxAge, c.Path)
				continue
			}
			switch c.Domain {
			case zone:
				onZone = true
			case "":
				hostOnly = true
			}
		}
		// Same name, Domain and Path as the one the browser holds, or the
		// browser keeps sending both.
		if !onZone {
			t.Errorf("this server's own %s on %s isn't expired", name, zone)
		}
		if !hostOnly {
			t.Errorf("a host-only %s from an older build isn't expired", name)
		}
	}
}

func TestSetSessionLeavesAnotherWorkspacesCookiesAlone(t *testing.T) {
	cloud(t)
	// Signed in to beta.onemana.dev (not yet updated) in the same browser: its
	// cookies are on the zone and arrive here too. Expiring them would sign the
	// person out of beta.
	rec := httptest.NewRecorder()
	SetSession(rec, request(
		&http.Cookie{Name: Access, Value: signed(t, theirKey, helpers.TokenTypeAccess, 5*time.Minute)},
		&http.Cookie{Name: Refresh, Value: signed(t, theirKey, helpers.TokenTypeRefresh, 24*time.Hour)},
		&http.Cookie{Name: Device, Value: "their-device"},
	), session())
	for name, cs := range written(rec) {
		for _, c := range cs {
			if c.Domain == zone {
				t.Errorf("%s on the zone was touched: it is another workspace's", name)
			}
		}
	}
}

func TestClearSessionExpiresOnlyThisServersCookies(t *testing.T) {
	cloud(t)
	rec := httptest.NewRecorder()
	ClearSession(rec, request(
		&http.Cookie{Name: hostAccess, Value: signed(t, ourKey, helpers.TokenTypeAccess, 5*time.Minute)},
		&http.Cookie{Name: hostRefr, Value: signed(t, ourKey, helpers.TokenTypeRefresh, time.Hour)},
		&http.Cookie{Name: hostDevice, Value: "device-1"},
		&http.Cookie{Name: Access, Value: signed(t, theirKey, helpers.TokenTypeAccess, 5*time.Minute)},
		&http.Cookie{Name: Refresh, Value: signed(t, theirKey, helpers.TokenTypeRefresh, time.Hour)},
		&http.Cookie{Name: Device, Value: "their-device"},
		&http.Cookie{Name: "om_analytics", Value: "storefront"},
	))
	got := written(rec)
	for _, name := range []string{hostAccess, hostRefr, hostDevice} {
		cs := got[name]
		if len(cs) != 1 || cs[0].MaxAge >= 0 || cs[0].Domain != "" || cs[0].Path != "/" || !cs[0].Secure {
			t.Errorf("%s isn't expired host-only, Secure, on / (%v)", name, cs)
		}
	}
	for name, cs := range got {
		for _, c := range cs {
			if c.Domain != "" {
				t.Errorf("signing out touched %s on %s, which isn't this workspace's", name, c.Domain)
			}
		}
	}
	if _, touched := got["om_analytics"]; touched {
		t.Error("signing out touched a cookie of the storefront's")
	}

	// This server's own old cookies go too.
	rec = httptest.NewRecorder()
	ClearSession(rec, request(
		&http.Cookie{Name: Refresh, Value: signed(t, ourKey, helpers.TokenTypeRefresh, time.Hour)},
		&http.Cookie{Name: Device, Value: "device-0"},
	))
	got = written(rec)
	for _, name := range []string{Refresh, Device} {
		found := false
		for _, c := range got[name] {
			found = found || (c.Domain == zone && c.MaxAge < 0)
		}
		if !found {
			t.Errorf("this server's own %s on the zone survived signing out", name)
		}
	}
	// A cookie the browser didn't send, it doesn't have (all were on Path=/):
	// no expiry for it.
	if len(got[Access]) != 0 {
		t.Errorf("an expiry for %s, which the browser doesn't hold", Access)
	}
}

func TestTheHostCookieIsReadFirst(t *testing.T) {
	cloud(t)
	own := signed(t, ourKey, helpers.TokenTypeAccess, 5*time.Minute)
	old := signed(t, ourKey, helpers.TokenTypeAccess, 5*time.Minute)
	r := request(&http.Cookie{Name: Access, Value: old}, &http.Cookie{Name: hostAccess, Value: own})
	if AccessToken(r) != own {
		t.Error("the old cookie was read over the host's own")
	}
}

func TestAnOldCookieIsReadOnlyWhenItIsThisServers(t *testing.T) {
	cloud(t)
	theirs := signed(t, theirKey, helpers.TokenTypeAccess, 5*time.Minute)
	ours := signed(t, ourKey, helpers.TokenTypeAccess, 5*time.Minute)
	// The browser sends the older cookie first; a server that read the first of
	// the name read another workspace's session.
	if got := AccessToken(request(&http.Cookie{Name: Access, Value: theirs}, &http.Cookie{Name: Access, Value: ours})); got != ours {
		t.Error("another workspace's token was read")
	}
	if got := AccessToken(request(&http.Cookie{Name: Access, Value: theirs})); got != "" {
		t.Error("another workspace's token was read when nothing of ours was there")
	}

	refresh := signed(t, ourKey, helpers.TokenTypeRefresh, time.Hour)
	if got := RefreshToken(request(&http.Cookie{Name: Refresh, Value: refresh})); got != refresh {
		t.Error("this server's old refresh cookie wasn't read")
	}
	if got := RefreshToken(request(&http.Cookie{Name: Refresh, Value: ours})); got != "" {
		t.Error("an access token was read as the refresh token")
	}
}

func TestTheDeviceFollowsTheSession(t *testing.T) {
	cloud(t)
	ours := signed(t, ourKey, helpers.TokenTypeRefresh, time.Hour)
	theirs := signed(t, theirKey, helpers.TokenTypeRefresh, time.Hour)
	if got := DeviceID(request(&http.Cookie{Name: Device, Value: "old"}, &http.Cookie{Name: hostDevice, Value: "new"})); got != "new" {
		t.Errorf("device %q, want the host's own", got)
	}
	if got := DeviceID(request(&http.Cookie{Name: Device, Value: "old"}, &http.Cookie{Name: Refresh, Value: ours})); got != "old" {
		t.Errorf("device %q, want this server's old one", got)
	}
	// A device id is opaque: only the session beside it says whose it is.
	if got := DeviceID(request(&http.Cookie{Name: Device, Value: "their-device"}, &http.Cookie{Name: Refresh, Value: theirs})); got != "" {
		t.Errorf("another workspace's device %q was read", got)
	}
}

func TestInDevelopmentTheNamesArePlainAndTheCookiesHostOnly(t *testing.T) {
	cloud(t)
	t.Setenv("COOKIE_SECURE", "false")
	t.Setenv("FE_DOMAIN", "localhost")
	ours := signed(t, ourKey, helpers.TokenTypeRefresh, time.Hour)
	rec := httptest.NewRecorder()
	SetSession(rec, request(&http.Cookie{Name: Refresh, Value: ours}), session())
	got := written(rec)
	for _, name := range []string{Access, Refresh, Device} {
		// Only the refresh cookie came with the request: an old one the browser
		// didn't send, it doesn't have.
		wantExpired := 0
		if name == Refresh {
			wantExpired = 1
		}
		var set, expiredOnDomain int
		for _, c := range got[name] {
			switch {
			case c.Domain == "" && c.MaxAge >= 0:
				set++
				if c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
					t.Errorf("%s: Secure=%v HttpOnly=%v SameSite=%v Path=%q", name, c.Secure, c.HttpOnly, c.SameSite, c.Path)
				}
			case c.Domain == "" && c.MaxAge < 0:
				t.Errorf("%s host-only expired in the answer that sets it", name)
			case c.MaxAge < 0:
				expiredOnDomain++
			}
		}
		if set != 1 {
			t.Errorf("%s set %d times host-only, want 1", name, set)
		}
		if expiredOnDomain != wantExpired {
			t.Errorf("this server's own %s on FE_DOMAIN expired %d times, want %d", name, expiredOnDomain, wantExpired)
		}
	}
}

func TestWriteIsHostOnly(t *testing.T) {
	cloud(t)
	rec := httptest.NewRecorder()
	at := time.Now().Add(time.Hour)
	Write(rec, CSRF, "value", at)
	cs := written(rec)["__Host-X-CSRF-Token"]
	if len(cs) != 1 || cs[0].Domain != "" || !cs[0].HttpOnly || !cs[0].Secure || cs[0].SameSite != http.SameSiteLaxMode || cs[0].Path != "/" {
		t.Fatalf("Write: %v", cs)
	}
}
