//go:build integration

package postgresInit

import "time"

// SetMigrationPollForTest sets how VerifySchemaAtStartup's wait for a fresh
// database's migrations sleeps between looks at the ledger, and returns what
// puts the real one back. It exists only in the integration build, so no
// product code can reach it.
func SetMigrationPollForTest(poll func(time.Duration) <-chan time.Time) (restore func()) {
	was := migrationPoll
	migrationPoll = poll
	return func() { migrationPoll = was }
}
