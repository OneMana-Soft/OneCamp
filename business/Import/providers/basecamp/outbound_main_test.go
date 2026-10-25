package basecamp

import "testing"

// allowAPI lets this test's fake Basecamp at origin be sent the token, in
// place of Basecamp's own API hosts.
func allowAPI(t *testing.T, origin string) {
	prev := apiOrigins
	apiOrigins = []string{origin}
	t.Cleanup(func() { apiOrigins = prev })
}
