//go:build integration

package models

// A project's rates against Postgres 12 with every migration: set, replace,
// read back, and clear.
// Run: go test -tags=integration ./models/postgres/ProjectRate/ -v

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestProjectRates(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	project, admin, maya, sam := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if b, err := Get(project); err != nil || b != nil {
		t.Fatalf("a project starts with no billing: %+v %v", b, err)
	}
	if err := Set(project, "USD", 5000, map[uuid.UUID]int64{maya: 9000, sam: 7000}, admin); err != nil {
		t.Fatal(err)
	}
	// Setting again replaces the people: Sam's own rate goes.
	if err := Set(project, "EUR", 6000, map[uuid.UUID]int64{maya: 9500}, admin); err != nil {
		t.Fatal(err)
	}
	b, err := Get(project)
	if err != nil || b == nil || b.Currency != "EUR" || b.DefaultRateCents != 6000 || len(b.People) != 1 || b.People[maya] != 9500 {
		t.Fatalf("read back: %+v %v", b, err)
	}
	if err := Set(project, "usd", 5000, nil, admin); err == nil {
		t.Fatal("a lowercase currency got past the check constraint")
	}
	if err := Clear(project); err != nil {
		t.Fatal(err)
	}
	if b, err := Get(project); err != nil || b != nil {
		t.Fatalf("cleared, with its people's rates: %+v %v", b, err)
	}
}
