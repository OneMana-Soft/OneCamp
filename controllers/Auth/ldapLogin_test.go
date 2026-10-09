package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	ldapService "github.com/akashc777/OneCamp/services/LDAP"
)

// A directory entry without an address is refused, and the person is told
// which attributes the directory is missing, so the admin knows what to add.
// The account is never made: nothing past the refusal (the database) is set up
// here, so reaching it would fail this test.
func TestDirectorySignInRefusesAnEntryWithoutAnAddress(t *testing.T) {
	old := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = old })
	helpers.SeatLimit = ""
	t.Setenv("LDAP_ENABLED", "true")
	t.Setenv("LDAP_HOST", "ldap.example.com")
	t.Setenv("LDAP_BASE_DN", "dc=example,dc=com")
	t.Setenv("LDAP_USER_FILTER", "(uid=%s)")
	defer UseDirectoryForTest(func(_ *ldapService.LDAPClient, _, _ string) (*ldapService.LDAPUser, error) {
		// What the directory client gives for an entry with neither mail nor
		// userPrincipalName (services/LDAP userFromEntry).
		return &ldapService.LDAPUser{DN: "uid=sam,ou=people,dc=example,dc=com", Username: "sam", FullName: "Sam"}, nil
	})()

	rec := httptest.NewRecorder()
	LDAPLogin(rec, httptest.NewRequest(http.MethodPost, "/auth/ldap-login",
		strings.NewReader(`{"username_or_email":"sam","password":"directory-password"}`)))

	var body struct{ Msg string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d (%s), want 401", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"no email address", "mail", "userPrincipalName"} {
		if !strings.Contains(body.Msg, want) {
			t.Errorf("the refusal %q doesn't say %q", body.Msg, want)
		}
	}
}

// LDAP_CA_CERT reaches the directory client a sign-in uses, and nothing is
// trusted beyond the system's without it.
func TestDirectorySignInTrustsLDAPCACert(t *testing.T) {
	old := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = old })
	helpers.SeatLimit = ""
	t.Setenv("LDAP_ENABLED", "true")
	t.Setenv("LDAP_HOST", "ldap.example.com")
	t.Setenv("LDAP_USE_TLS", "true")
	t.Setenv("LDAP_BASE_DN", "dc=example,dc=com")
	t.Setenv("LDAP_USER_FILTER", "(uid=%s)")
	var used string
	defer UseDirectoryForTest(func(c *ldapService.LDAPClient, _, _ string) (*ldapService.LDAPUser, error) {
		used = c.CACertPath
		return nil, errors.New("stand-in directory: stop here")
	})()
	signIn := func() {
		LDAPLogin(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/auth/ldap-login",
			strings.NewReader(`{"username_or_email":"sam","password":"directory-password"}`)))
	}

	t.Setenv("LDAP_CA_CERT", "/app/ldap/ca.pem")
	signIn()
	if used != "/app/ldap/ca.pem" {
		t.Fatalf("the directory client was given CA bundle %q, want /app/ldap/ca.pem", used)
	}
	t.Setenv("LDAP_CA_CERT", "")
	signIn()
	if used != "" {
		t.Fatalf("without LDAP_CA_CERT the directory client was given %q", used)
	}
}
