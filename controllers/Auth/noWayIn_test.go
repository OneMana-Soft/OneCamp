package controllers

import (
	"strings"
	"testing"
)

// With passwords off and no other way in set up, members can't sign in at
// all, and nothing said so. The admin's system check and the boot log do now;
// any other way in, or passwords, is enough.
func TestNobodyCanSignInIsSaid(t *testing.T) {
	if problem := noWayIn(map[string]bool{"email": false, "demo": true}); !strings.Contains(problem, "members can't sign in at all") {
		t.Errorf("passwords off and nothing else: %q", problem)
	}
	for _, methods := range []map[string]bool{
		{"email": true},
		{"email": false, "google": true},
		{"email": false, "saml": true},
		{"email": false, "ldap": true},
	} {
		if problem := noWayIn(methods); problem != "" {
			t.Errorf("%v: %q, want nothing to say", methods, problem)
		}
	}
}
