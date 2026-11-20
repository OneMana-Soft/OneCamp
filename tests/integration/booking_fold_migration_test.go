//go:build integration
// +build integration

package integration_test

// Booking questions (gcal-7 H3) and team hosts (gcal-7 H4) are one migration
// pair: 608_booking_team_hosts was folded into 607_booking_questions before
// it shipped (the migration lint's L6). Against real Postgres 12 the pair
// goes up, down and up again, its up runs twice, and its down takes away
// everything both halves added.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestBookingQuestionsAndTeamHostsMigrationUpDownUp' -v

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/akashc777/OneCamp/tests/integration"
)

// bookingFoldMigration is the folded pair in one direction, found by name.
func bookingFoldMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	migrations := filepath.Join(filepath.Dir(here), "..", "..", "migrations")
	if gone, _ := filepath.Glob(filepath.Join(migrations, "*_booking_team_hosts.*.sql")); len(gone) != 0 {
		t.Fatalf("booking_team_hosts is folded into booking_questions, found %v", gone)
	}
	matches, _ := filepath.Glob(filepath.Join(migrations, "*_booking_questions."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one booking_questions %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBookingQuestionsAndTeamHostsMigrationUpDownUp(t *testing.T) {
	up, down := bookingFoldMigration(t, "up"), bookingFoldMigration(t, "down")
	env := integration.SetupEnv(t)
	exists := func(q string, args ...any) bool {
		t.Helper()
		var ok bool
		if err := env.PG.QueryRow(q, args...).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		for _, c := range [][2]string{
			{"booking_pages", "questions"}, {"bookings", "answers"}, // 607's own
			{"booking_pages", "team_mode"}, {"bookings", "host_id"}, // folded from 608
		} {
			if got := columnExists(t, env, c[0], c[1]); got != step.want {
				t.Fatalf("step %d: %s.%s exists = %v", i, c[0], c[1], got)
			}
		}
		if got := tableExists(t, env.PG, "booking_page_hosts"); got != step.want {
			t.Fatalf("step %d: booking_page_hosts exists = %v", i, got)
		}
		if got := exists(`SELECT to_regclass('idx_booking_page_hosts_user') IS NOT NULL`); got != step.want {
			t.Fatalf("step %d: idx_booking_page_hosts_user exists = %v", i, got)
		}
		// Once, never twice: the up drops the named check before adding it.
		var checks int
		if err := env.PG.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conname = 'booking_pages_team_mode_check'`).Scan(&checks); err != nil {
			t.Fatal(err)
		}
		if want := map[bool]int{true: 1, false: 0}[step.want]; checks != want {
			t.Fatalf("step %d: booking_pages_team_mode_check count = %d, want %d", i, checks, want)
		}
	}
	if !exists(`SELECT pg_get_constraintdef(oid) LIKE '%round_robin%' FROM pg_constraint WHERE conname = 'booking_pages_team_mode_check'`) {
		t.Fatal("team_mode's check names solo, collective and round_robin")
	}
}
