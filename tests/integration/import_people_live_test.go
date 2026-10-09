//go:build integration
// +build integration

package integration_test

// The import's offer of people to invite counts only a LIVE invitation as
// "already invited" (models.InvitationLiveSQL, the one rule): someone whose
// invitation expired is offered again. Any invitation row at all used to
// count, so they never were.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAnExpiredInvitationsPersonIsOfferedAgain|TestInvitationLiveSQLIsLiveAt' -v

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// invitationRows are one invitation of each kind LiveAt tells apart.
var invitationRows = []struct {
	email, status, expires, made string
	live                         bool
}{
	{"live@acme.test", "sent", "NOW() + interval '3 days'", "NOW()", true},
	{"expired@acme.test", "sent", "NOW() - interval '1 day'", "NOW() - interval '8 days'", false},
	{"recent@acme.test", "pending", "NULL", "NOW() - interval '2 days'", true},
	{"old@acme.test", "pending", "NULL", "NOW() - interval '8 days'", false},
	{"used@acme.test", "joined", "NOW() + interval '3 days'", "NOW()", false},
	{"marked@acme.test", "expired", "NOW() + interval '3 days'", "NOW()", false},
}

func TestAnExpiredInvitationsPersonIsOfferedAgain(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	admin, job := uuid.New(), uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id, email_id, username) VALUES ($1, 'admin@acme.test', 'admin')`, admin)
	exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, started_at, completed_at, triggered_by)
		VALUES ($1, 'asana', 'Acme', 'api', 'completed', NOW() - interval '1 hour', NOW(), $2)`, job, admin)
	for i, row := range invitationRows {
		id := uuid.New()
		exec(`INSERT INTO users (id, email_id, display_name, is_external) VALUES ($1, $2, $3, true)`, id, row.email, row.email)
		exec(`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid) VALUES ($1, 'user', $2, $3)`,
			job, "src-"+string(rune('a'+i)), id)
		exec(`INSERT INTO invitations (email, invited_by, status, token, token_expires_at, created_at)
			VALUES ($1, $2, $3, $4, `+row.expires+`, `+row.made+`)`, row.email, admin, row.status, "tok-"+row.email)
	}

	people, err := importModels.ListImportedPeople(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	invited := map[string]bool{}
	for _, p := range people {
		invited[p.Email] = p.Invited
	}
	for _, row := range invitationRows {
		if invited[row.email] != row.live {
			t.Errorf("%s (%s): counted as already invited %v, want %v", row.email, row.status, invited[row.email], row.live)
		}
	}
}

// The SQL condition and LiveAt are one rule written twice, side by side: they
// answer the same for every kind of invitation.
func TestInvitationLiveSQLIsLiveAt(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	for _, row := range invitationRows {
		if _, err := env.PG.Exec(`INSERT INTO invitations (email, status, token, token_expires_at, created_at)
			VALUES ($1, $2, $3, `+row.expires+`, `+row.made+`)`, row.email, row.status, "tok-"+row.email); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := env.PG.Query(`SELECT i.email, i.status, i.token_expires_at, i.created_at, ` + userModels.InvitationLiveSQL("i") + ` FROM invitations i`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	now := time.Now()
	seen := 0
	for rows.Next() {
		var inv userModels.Invitation
		var bySQL bool
		if err := rows.Scan(&inv.Email, &inv.Status, &inv.TokenExpiresAt, &inv.CreatedAt, &bySQL); err != nil {
			t.Fatal(err)
		}
		if byGo := inv.LiveAt(now); byGo != bySQL {
			t.Errorf("%s: LiveAt says %v, the SQL condition %v", inv.Email, byGo, bySQL)
		}
		seen++
	}
	if seen != len(invitationRows) {
		t.Fatalf("checked %d of %d invitations", seen, len(invitationRows))
	}
}
