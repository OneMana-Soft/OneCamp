package helpers

import (
	"os"
	"regexp"
	"testing"
)

// LDAP_CA_CERT names a file the API reads, and the API runs in a container
// that sees only what is mounted into it (the image leaves *.pem files out).
// The shipped compose file mounts ./ldap for it, read-only; an operator's own
// edit to that file would be lost at the next update, which replaces it.
func TestLDAPCACertHasAPlaceInTheAPIContainer(t *testing.T) {
	compose, err := os.ReadFile("../distribute-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	api := regexp.MustCompile(`(?ms)^  go-service:\n(.*?)^  [a-z-]+:\n`).FindSubmatch(compose)
	if api == nil {
		t.Fatal("no go-service in the shipped compose file")
	}
	if !regexp.MustCompile(`(?m)^\s+- \./ldap:/app/ldap:ro$`).Match(api[1]) {
		t.Error("the API service doesn't mount ./ldap at /app/ldap, where LDAP_CA_CERT's file is read")
	}
}
