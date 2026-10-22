package controllers

// Signing out of one workspace clears that workspace's cookies and nothing
// else. It used to expire the session names on FE_DOMAIN whatever they held,
// then every cookie the request carried, on FE_DOMAIN: on Cloud that is the
// zone, so signing out of acme also signed the person out of beta (whose
// cookies sat on the zone) and deleted the storefront's cookies.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
)

func signedWith(t *testing.T, key, typ string) string {
	t.Helper()
	t.Setenv("JWT_SECRET", key)
	s, err := helpers.SignSessionToken(uuid.NewString(), typ, time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func signOut(cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/logout", nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	LogOutUser(rec, r)
	return rec
}

func TestSigningOutClearsOnlyThisWorkspacesCookies(t *testing.T) {
	t.Setenv("FE_DOMAIN", "onemana.dev")
	t.Setenv("FE_HOST_DOMAIN", "acme.onemana.dev")
	t.Setenv("BACKEND_DOMAIN", "acme-api.onemana.dev")
	t.Setenv("COOKIE_SECURE", "")
	theirAccess := signedWith(t, "beta-workspace-secret-0123456789abcdef", helpers.TokenTypeAccess)
	theirRefresh := signedWith(t, "beta-workspace-secret-0123456789abcdef", helpers.TokenTypeRefresh)
	ourRefresh := signedWith(t, "acme-workspace-secret-0123456789abcdef", helpers.TokenTypeRefresh)

	rec := signOut(
		&http.Cookie{Name: "Authorization", Value: theirAccess},
		&http.Cookie{Name: "RefreshToken", Value: theirRefresh},
		&http.Cookie{Name: "DeviceId", Value: "their-device"},
		&http.Cookie{Name: "om_analytics", Value: "storefront"},
		&http.Cookie{Name: "__Host-RefreshToken", Value: ourRefresh},
		&http.Cookie{Name: "__Host-DeviceId", Value: "our-device"},
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("signing out answered %d", rec.Code)
	}
	expired := map[string]bool{}
	for _, c := range rec.Result().Cookies() {
		if c.Domain != "" {
			t.Errorf("signing out touched %s on %s, which isn't this workspace's", c.Name, c.Domain)
		}
		if c.Name == "om_analytics" {
			t.Error("signing out touched the storefront's cookie")
		}
		if c.MaxAge < 0 && c.Domain == "" && c.Path == "/" && c.Secure {
			expired[c.Name] = true
		}
	}
	for _, name := range []string{"__Host-Authorization", "__Host-RefreshToken", "__Host-DeviceId"} {
		if !expired[name] {
			t.Errorf("%s survived signing out", name)
		}
	}
}

func TestSigningOutClearsThisServersOwnOldCookies(t *testing.T) {
	t.Setenv("FE_DOMAIN", "onemana.dev")
	t.Setenv("FE_HOST_DOMAIN", "acme.onemana.dev")
	t.Setenv("BACKEND_DOMAIN", "acme-api.onemana.dev")
	t.Setenv("COOKIE_SECURE", "")
	ourRefresh := signedWith(t, "acme-workspace-secret-0123456789abcdef", helpers.TokenTypeRefresh)
	rec := signOut(&http.Cookie{Name: "RefreshToken", Value: ourRefresh}, &http.Cookie{Name: "DeviceId", Value: "our-old-device"})
	onZone := map[string]bool{}
	for _, c := range rec.Result().Cookies() {
		if c.Domain == "onemana.dev" && c.MaxAge < 0 && c.Path == "/" {
			onZone[c.Name] = true
		}
	}
	// The ones the browser sent; one it didn't send, it doesn't have.
	for _, name := range []string{"RefreshToken", "DeviceId"} {
		if !onZone[name] {
			t.Errorf("this server's own %s on the zone survived signing out", name)
		}
	}
}
