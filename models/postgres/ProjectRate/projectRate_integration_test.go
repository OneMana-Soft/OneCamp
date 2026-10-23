//go:build integration

package models

// A project's rates against Postgres 12 with every migration: set, replace,
// read back, and clear.
// Run: go test -tags=integration ./models/postgres/ProjectRate/ -v

import (
	"context"
	"errors"
	"testing"
	"time"

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
	first, err := Set(project, "USD", 5000, map[uuid.UUID]int64{maya: 9000, sam: 7000}, time.Time{}, admin)
	if err != nil {
		t.Fatal(err)
	}
	// Setting again from that read replaces the people: Sam's own rate goes.
	if _, err := Set(project, "EUR", 6000, map[uuid.UUID]int64{maya: 9500}, first, admin); err != nil {
		t.Fatal(err)
	}
	b, err := Get(project)
	if err != nil || b == nil || b.Currency != "EUR" || b.DefaultRateCents != 6000 || len(b.People) != 1 || b.People[maya] != 9500 {
		t.Fatalf("read back: %+v %v", b, err)
	}
	if _, err := Set(project, "usd", 5000, nil, b.UpdatedAt, admin); err == nil {
		t.Fatal("a lowercase currency got past the check constraint")
	}
	if err := Clear(project); err != nil {
		t.Fatal(err)
	}
	if b, err := Get(project); err != nil || b != nil {
		t.Fatalf("cleared, with its people's rates: %+v %v", b, err)
	}
}

// S1 (the 30-day audit): a save carries the version of the rates it read. A
// save from a read that failed (no version), one that crossed another
// admin's, or one from rates cleared since, is refused with ErrChanged and
// writes nothing. Before, it replaced the currency and deleted every
// person's own rate.
func TestARatesSaveFromAStaleReadChangesNothing(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	project, admin, maya, sam := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	read, err := Set(project, "INR", 150000, map[uuid.UUID]int64{maya: 200000, sam: 180000}, time.Time{}, admin)
	if err != nil {
		t.Fatal(err)
	}
	// Another admin saves after this one read: Maya's rate goes up.
	theirs, err := Set(project, "INR", 160000, map[uuid.UUID]int64{maya: 210000, sam: 180000}, read, admin)
	if err != nil {
		t.Fatal(err)
	}
	for name, from := range map[string]time.Time{"a read that failed": {}, "a stale read": read} {
		if _, err := Set(project, "USD", 5000, nil, from, admin); !errors.Is(err, ErrChanged) {
			t.Fatalf("a save from %s: %v, want ErrChanged", name, err)
		}
	}
	b, err := Get(project)
	if err != nil || b == nil || b.Currency != "INR" || b.DefaultRateCents != 160000 || len(b.People) != 2 || b.People[maya] != 210000 || !b.UpdatedAt.Equal(theirs) {
		t.Fatalf("a refused save changed the rates: %+v %v", b, err)
	}
	if err := Clear(project); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(project, "INR", 160000, nil, theirs, admin); !errors.Is(err, ErrChanged) {
		t.Fatalf("a save from rates cleared since: %v, want ErrChanged", err)
	}
}
