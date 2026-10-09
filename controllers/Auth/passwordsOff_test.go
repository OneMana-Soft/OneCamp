package controllers

import "testing"

// Someone sending a password to a workspace that turned passwords off is told
// the ways in it does have, and only those: it used to name Google, GitHub and
// single sign-on whatever was set up.
func TestPasswordsOffNamesOnlyTheWaysIn(t *testing.T) {
	for _, c := range []struct {
		providers map[string]bool
		joining   bool
		want      string
	}{
		{map[string]bool{"google": true}, false, "This workspace doesn't use passwords. Sign in with Google instead."},
		{map[string]bool{"github": true, "saml": true}, false, "This workspace doesn't use passwords. Sign in with GitHub or single sign-on instead."},
		{map[string]bool{"google": true, "github": true, "oidc": true, "ldap": true}, false,
			"This workspace doesn't use passwords. Sign in with Google, GitHub, single sign-on or your directory account instead."},
		{map[string]bool{"oidc": true}, true, "This workspace doesn't use passwords. Join with single sign-on, using the address you were invited at."},
		{map[string]bool{"email": false, "demo": true}, false, "This workspace doesn't use passwords, and no other way to sign in is set up yet. Ask your administrator."},
	} {
		if got := passwordsOffMessage(c.providers, c.joining); got != c.want {
			t.Errorf("%v (joining %v): %q, want %q", c.providers, c.joining, got, c.want)
		}
	}
}
