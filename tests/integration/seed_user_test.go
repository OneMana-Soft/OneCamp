//go:build integration

package integration_test

// seedUser's home on this edition: the AI edition keeps it in its
// mcp_approval_test.go, which this edition doesn't have, and the shared
// notify_why_test.go calls it. The code is the AI edition's as it is.

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
)

// seedUser inserts the minimum users row the pending-action foreign key needs and
// returns its id.
func seedUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	id := uuid.NewString()
	// Only id and email_id: everything else on users is nullable or defaulted, and
	// inserting more would couple this test to columns later migrations add.
	if _, err := db.Exec(
		`INSERT INTO users (id, email_id) VALUES ($1, $2)`,
		id, id[:8]+"@example.test",
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}
