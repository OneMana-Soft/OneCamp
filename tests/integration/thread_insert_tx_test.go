//go:build integration
// +build integration

package integration_test

// A thread written inside a transaction goes with it (V-281). Insert and
// TakeBack both follow the transaction a caller carries on ctx, so a create
// that runs in one and fails leaves no empty thread behind, and a thread is
// seen outside its transaction only once that commits.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAThreadWrittenInATransactionGoesWithIt' -v

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	threadModel "github.com/akashc777/OneCamp/models/postgres/Thread"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAThreadWrittenInATransactionGoesWithIt(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	newRow := func() *threadModel.Row {
		now := time.Now().UTC()
		return &threadModel.Row{ID: uuid.New(), ResourceKind: "doc", ResourceUUID: uuid.NewString(), AnchorKind: "none",
			Status: "open", CreatedBy: uuid.New(), CreatedAt: now, UpdatedAt: now}
	}
	// seen counts the thread from outside any transaction.
	seen := func(id uuid.UUID) (int, error) {
		var n int
		err := env.PG.QueryRow(`SELECT count(*) FROM threads WHERE id = $1`, id).Scan(&n)
		return n, err
	}
	failed := errors.New("the first comment couldn't be written")

	t.Run("a create that fails inside a transaction takes its thread back with it", func(t *testing.T) {
		r := newRow()
		var inserted bool
		err := pgexec.InTx(ctx, func(ctx context.Context) error {
			var err error
			if inserted, err = threadModel.Insert(ctx, r, false); err != nil {
				return err
			}
			if err := threadModel.TakeBack(ctx, r.ID); err != nil {
				return err
			}
			return failed
		})
		if !inserted || !errors.Is(err, failed) {
			t.Fatalf("the create in a transaction: inserted %v, ended with %v; want it inserted, then rolled back by its own failure", inserted, err)
		}
		if n, err := seen(r.ID); err != nil || n != 0 {
			t.Fatalf("after the transaction rolled back: %d thread rows (%v); want none, not an empty thread left behind", n, err)
		}
	})

	t.Run("a thread written in a transaction is seen outside it only once that commits", func(t *testing.T) {
		r := newRow()
		before := -1
		err := pgexec.InTx(ctx, func(ctx context.Context) error {
			if _, err := threadModel.Insert(ctx, r, false); err != nil {
				return err
			}
			var err error
			before, err = seen(r.ID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if before != 0 {
			t.Fatalf("before the commit, %d thread rows seen outside the transaction; want none", before)
		}
		if n, err := seen(r.ID); err != nil || n != 1 {
			t.Fatalf("after the commit: %d thread rows (%v); want the one", n, err)
		}
	})
}
