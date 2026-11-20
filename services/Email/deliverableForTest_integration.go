//go:build integration

package email

import "testing"

// LetReservedNamesThroughForTest lets mail to a name reserved for examples
// (example.com, example.org, .test, .example ...) through Deliverable until
// the test ends, so an integration test that catches mail with a fake
// SharedHTTPClient can address it to alex@example.com instead of a real
// provider's address. A reserved name still skips the MX lookup, so nothing
// asks the DNS; a real domain is checked as before. It exists only in the
// integration build: the tag keeps it out of the shipped binary, where
// Deliverable refuses every reserved name.
//
// Each test opts in for itself, and t.Cleanup puts the refusal back, so the
// tests that prove a reserved address is never mailed keep proving it.
func LetReservedNamesThroughForTest(t testing.TB) {
	t.Helper()
	was := reservedReachTestTransport.Swap(true)
	t.Cleanup(func() { reservedReachTestTransport.Store(was) })
}
