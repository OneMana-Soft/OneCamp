//go:build integration

package business

// ActorFor: the person by id as Postgres holds them now, for work that runs
// without their request (an import).
// Run: go test -tags=integration ./business/DataTable/ -run TestActorForIsThePersonAsTheyAreNow -v

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestActorForIsThePersonAsTheyAreNow(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	ada, maya, gone := uuid.New(), uuid.New(), uuid.New()
	for id, email := range map[uuid.UUID]string{ada: "ada@example.test", maya: "maya@example.test", gone: "gone@example.test"} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, email, email, email); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id) VALUES ('ada@example.test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}

	if a, err := ActorFor(ctx, ada, rowLedger.ActorImport); err != nil || a.UserID != ada || !a.IsAdmin || a.Kind != rowLedger.ActorImport {
		t.Errorf("Ada, an admin: %+v, %v", a, err)
	}
	if a, err := ImportActor(ctx, maya); err != nil || a.UserID != maya || a.IsAdmin || a.Kind != rowLedger.ActorImport {
		t.Errorf("Maya, a member: %+v, %v", a, err)
	}
	// Work an API token asked for acts as a member, whoever owns the token.
	if a, err := ActorFor(helpers.WithActsThroughToken(ctx), ada, rowLedger.ActorAPI); err != nil || a.IsAdmin {
		t.Errorf("Ada through a token: %+v, %v", a, err)
	}
	// A person removed, or never one, is no actor.
	for name, id := range map[string]uuid.UUID{"removed": gone, "never a member": uuid.New()} {
		if a, err := ActorFor(ctx, id, rowLedger.ActorImport); !errors.Is(err, errNotActive) {
			t.Errorf("a person %s: %+v, %v; want not active", name, a, err)
		}
	}
}
