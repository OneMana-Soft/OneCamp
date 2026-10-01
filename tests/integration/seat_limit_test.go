//go:build integration
// +build integration

package integration_test

// A free licence covers a set number of people. Every way a person joins
// meets the same check, bots and external identities never count, and a
// person who left frees their seat.

import (
	"context"
	"testing"
	"time"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestFreeLicenceSeatLimit(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	old := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = old })

	// Unlimited: anything goes.
	helpers.SeatLimit = ""
	first := uuid.New()
	if err := userDomain.CreateUser(ctx, "a@example.test", first); err != nil {
		t.Fatalf("unlimited: %v", err)
	}

	helpers.SeatLimit = "2"
	second := uuid.New()
	if err := userDomain.CreateUserWithMethod(ctx, "b@example.test", "b", nil, second, "", false); err != nil {
		t.Fatalf("the second seat is free: %v", err)
	}
	// Bots and external identities are not people on the licence.
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, is_bot, is_external) VALUES ($1, 'bot@example.test', true, true)`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if used, limit, err := userDomain.SeatUsage(ctx); err != nil || used != 2 || limit != 2 {
		t.Fatalf("usage = %d/%d (%v), want 2/2", used, limit, err)
	}

	// Full: both member inserts refuse, with the seat-limit error.
	if err := userDomain.CreateUser(ctx, "c@example.test", uuid.New()); !helpers.IsSeatLimit(err) {
		t.Fatalf("a third member must be refused, got %v", err)
	}
	if err := userDomain.CreateUserWithMethod(ctx, "d@example.test", "d", nil, uuid.New(), "", false); !helpers.IsSeatLimit(err) {
		t.Fatalf("SSO/SCIM/sign-up path must be refused too, got %v", err)
	}

	// Someone leaves: their seat is free again.
	now := time.Now()
	if err := userDomain.UpdateDeletedTimeByUUID(ctx, &now, &now, second); err != nil {
		t.Fatal(err)
	}
	third := uuid.New()
	if err := userDomain.CreateUser(ctx, "e@example.test", third); err != nil {
		t.Fatalf("a freed seat can be taken: %v", err)
	}
	// And the one who left cannot come back while the plan is full.
	if err := userDomain.UpdateDeletedTimeToNullByUUID(ctx, &now, second); !helpers.IsSeatLimit(err) {
		t.Fatalf("reactivating into a full plan must be refused, got %v", err)
	}
	// Reactivating someone already active adds no one and is allowed.
	if err := userDomain.UpdateDeletedTimeToNullByUUID(ctx, &now, first); err != nil {
		t.Fatalf("an active member is not a new seat: %v", err)
	}
}
