//go:build integration

package domain

// The one statement that makes preference rows (makeDefaultRows) keeps a row
// someone made first exactly as stored: the person's saved switches, and the
// token their first email already carries (CR-449 F1).
//
// Run: PKGS=./domain/UserNotificationPreference/ (go test -tags=integration ./domain/UserNotificationPreference/ -run 'TestAMadeRowKeepsWhatWasSaved' -v)

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	models "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAMadeRowKeepsWhatWasSaved(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	person := func(name string) uuid.UUID {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, id, name+"@example.test"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	rows := func(id uuid.UUID) (n int) {
		if err := env.PG.QueryRow(`SELECT count(*) FROM users_notification_preferences WHERE user_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("a row made meanwhile is returned and kept as saved", func(t *testing.T) {
		bo := person("bo")
		if _, err := env.PG.Exec(`INSERT INTO users_notification_preferences (user_id, email_enabled, email_dms, unsubscribe_token)
			VALUES ($1, false, false, 'tok-saved')`, bo); err != nil {
			t.Fatal(err)
		}
		// As a Dispatch whose read missed bo's row would make it.
		made, err := pgexec.Query(ctx, makeDefaultRows(), models.Scan, pgexec.Array([]string{bo.String()}), pgexec.Array([]string{"tok-new"}))
		if err != nil || len(made) != 1 {
			t.Fatalf("made %d rows, err %v; want bo's one", len(made), err)
		}
		stored, err := LoadByUserIDs(ctx, []uuid.UUID{bo})
		if err != nil || stored[bo] == nil {
			t.Fatalf("bo's stored row: %v", err)
		}
		for what, p := range map[string]*models.UserNotificationPreference{"returned": made[0], "stored": stored[bo]} {
			if p.EmailEnabled || p.EmailDMs || p.UnsubscribeToken != "tok-saved" {
				t.Errorf("the %s row: email %v, DMs %v, token %q; want off, off, the saved token", what, p.EmailEnabled, p.EmailDMs, p.UnsubscribeToken)
			}
		}
		if n := rows(bo); n != 1 {
			t.Errorf("bo has %d rows, want 1", n)
		}
	})

	t.Run("eight at once make one row, one token", func(t *testing.T) {
		cy := person("cy")
		const n = 8
		start := make(chan struct{})
		tokens, errs := make([]string, n), make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				p, err := LoadOrCreate(ctx, cy)
				if errs[i] = err; err == nil {
					tokens[i] = p.UnsubscribeToken
				}
			}()
		}
		close(start)
		wg.Wait()
		for i := range n {
			if errs[i] != nil || tokens[i] == "" || tokens[i] != tokens[0] {
				t.Errorf("call %d: token %q, err %v; want the one token %q", i, tokens[i], errs[i], tokens[0])
			}
		}
		if got := rows(cy); got != 1 {
			t.Errorf("cy has %d rows, want 1", got)
		}
	})
}
