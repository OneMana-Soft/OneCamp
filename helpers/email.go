package helpers

import (
	"strings"
	"unicode/utf8"
)

// NormalizeEmail is how an address is kept: trimmed, with its ASCII capitals
// A to Z made lowercase, and nothing else changed.
//
// Every way an address arrives (SCIM, Google, GitHub, OIDC, SAML, LDAP, an
// invitation, a sign-up) goes through it before an account is made or found,
// because mail treats "Ana@Example.com" and "ana@example.com" as one mailbox
// and a person who signs in with one after being invited at the other must
// reach the same account. Lookups compare without case as well, for the
// accounts made before this, which keep the case they arrived with.
//
// ONLY A TO Z. Unicode lowercasing turns characters that are not those letters
// into ones that are: the Kelvin sign (U+212A) becomes "k", so
// "Kate@example.com" was kate@example.com, and the capital I with a dot
// (U+0130) becomes "i" and a combining dot. An address with anything outside
// ASCII is not matched to an account or admitted at all (AddressIsASCII), so
// the lowercasing never has to decide what such a character means. Pure.
func NormalizeEmail(email string) string {
	b := []byte(strings.TrimSpace(email))
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// AddressIsASCII reports whether an address is written in ASCII alone, the
// only kind matched to an account or admitted: see NormalizeEmail. Pure.
func AddressIsASCII(email string) bool {
	for i := 0; i < len(email); i++ {
		if email[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
