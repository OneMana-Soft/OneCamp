//go:build integration

package models

import "github.com/akashc777/OneCamp/helpers"

// SetStartRetriesForTest sets how HashLeftInClearAtStart tries again, and
// returns what puts the real ones back. It exists only in the integration
// build, so no product code can reach it.
func SetStartRetriesForTest(r helpers.Retries) (restore func()) {
	was := startRetries
	startRetries = r
	return func() { startRetries = was }
}
