//go:build integration
// +build integration

package integration_test

// A new approval pushes to the person's phone once. An MCP client retries a
// waiting call until it is decided, and each retry reaches CreatePendingAction
// with the same idempotency key; if the store reported those as new, every
// retry would buzz the person again.

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestPendingActionIsFreshOnlyWhenInserted(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := uuid.MustParse(seedUser(t, env.PG))
	key := "fresh-" + uuid.NewString()
	create := func(k string) *pendingModels.PendingAction {
		t.Helper()
		a, err := pendingModels.CreatePendingAction(ctx, owner, "mcp", "", "update_task_status",
			map[string]string{"task_uuid": uuid.NewString()}, "Mark it done", k, time.Now().Add(time.Hour), pendingModels.Attribution{})
		if err != nil || a == nil {
			t.Fatalf("create: %v", err)
		}
		return a
	}

	first := create(key)
	if !first.Fresh {
		t.Fatal("the call that inserted the approval must report it as fresh")
	}
	retry := create(key)
	if retry.Fresh || retry.Id != first.Id {
		t.Fatalf("a retry must get the same approval back, not fresh: fresh=%v same=%v", retry.Fresh, retry.Id == first.Id)
	}
	if other := create("fresh-" + uuid.NewString()); !other.Fresh {
		t.Fatal("a different proposal is a new approval")
	}
	if noKey := create(""); !noKey.Fresh {
		t.Fatal("a proposal without a key is always new")
	}
}
