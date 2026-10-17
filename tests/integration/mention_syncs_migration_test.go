//go:build integration
// +build integration

package integration_test

// Migration 344 goes up, down and up again on Postgres 12, and what migration
// 210 had kept already counts: each thing with a kept mention has its first
// keeping, and docs' mentions are kept since the first of them (or from the
// migration, when there are none).
//
// Run: go test -tags=integration ./tests/integration/ -run TestResourceMentionSyncsMigrationUpDownUp -v

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/tests/integration"
)

func TestResourceMentionSyncsMigrationUpDownUp(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(here), "..", "..", "migrations")
	read := func(direction string) (string, string) {
		matches, _ := filepath.Glob(filepath.Join(dir, "*_resource_mention_syncs."+direction+".sql"))
		if len(matches) != 1 {
			t.Fatalf("want one resource_mention_syncs %s migration, found %v", direction, matches)
		}
		b, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Base(matches[0]), string(b)
	}
	upName, up := read("up")
	_, down := read("down")
	if n, err := strconv.Atoi(strings.SplitN(upName, "_", 2)[0]); err != nil || n <= 210 || n == 198 {
		t.Fatalf("%s: it reads what 210 made, so it comes after it, and is never 198", upName)
	}
	if strings.Contains(up, "gen_random_uuid") || strings.Contains(up, "uuid_generate") {
		t.Fatalf("%s makes ids in the database; Postgres 12 has neither function by default", upName)
	}

	env := integration.SetupEnv(t)
	exists := func() bool {
		return tableExists(t, env.PG, "resource_mention_syncs") && tableExists(t, env.PG, "resource_mention_tracking")
	}
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		if exists() != step.want {
			t.Fatalf("step %d: the tables exist = %v", i, !step.want)
		}
	}

	// Up again over a database 210 has kept mentions in.
	doc, other := uuid.NewString(), uuid.NewString()
	first := time.Date(2026, 10, 13, 5, 0, 0, 0, time.UTC)
	for _, r := range []struct {
		doc string
		at  time.Time
	}{{doc, first.Add(time.Hour)}, {doc, first.Add(2 * time.Hour)}, {other, first}} {
		if _, err := env.PG.Exec(`INSERT INTO resource_mentions (resource_kind, resource_uuid, user_uuid, mentioned_at) VALUES ('doc', $1, $2, $3)`,
			r.doc, uuid.New(), r.at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.PG.Exec(up); err != nil {
		t.Fatalf("up again: %v", err)
	}
	var kept int
	if err := env.PG.QueryRow(`SELECT COUNT(*) FROM resource_mention_syncs WHERE resource_kind = 'doc' AND resource_uuid IN ($1, $2)`, doc, other).Scan(&kept); err != nil || kept != 2 {
		t.Fatalf("first keepings of what 210 kept: %d, %v", kept, err)
	}
	var since time.Time
	if err := env.PG.QueryRow(`SELECT since FROM resource_mention_tracking WHERE resource_kind = 'doc'`).Scan(&since); err != nil || !since.Equal(first) {
		t.Fatalf("docs' mentions kept since %v (%v), want %v", since, err, first)
	}
	// And up once more changes nothing.
	if _, err := env.PG.Exec(up); err != nil {
		t.Fatalf("up a third time: %v", err)
	}
}
