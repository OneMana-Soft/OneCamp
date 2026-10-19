//go:build integration

package business

// A team's search follows its archive in the order it was archived and
// restored, against a real Postgres and Dgraph.
//
// Archiving a team marks it, its projects and tasks, and its AI embeddings,
// deleted in search; restoring it clears the marks. Each started on
// goroutines of their own, so a restore whose clear finished before the
// archive's mark left a live team's whole content out of search. Modelled on
// the channels' (c9c26b3a).
//
// Run: go test -tags=integration ./business/Team/ -run TestATeamsSearchFollowsItsArchiveInOrder -v

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// searchStep is one change asked of search: archived or live again, starting
// or ending.
type searchStep struct {
	archived, ended bool
}

func TestATeamsSearchFollowsItsArchiveInOrder(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	ana, launch := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'ana@example.test', 'ana', 'ana', NOW(), NOW())`, ana); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Launch', $2)`, launch, ana); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": ana.String(), "user_name": "ana"},
		{"uid": "_:launch", "dgraph.type": "Team", "team_uuid": launch.String(), "team_name": "Launch",
			"team_deleted_at": "0001-01-01T00:00:00Z", "team_members": []map[string]any{{"uid": "_:ana"}},
			"team_admins": []map[string]any{{"uid": "_:ana"}}},
	})
	info := &dgraphStruct.DgraphTeam{Uuid: launch.String(), Name: "Launch"}

	// Every change asked of search, as it starts and as it ends. The archive's
	// holds until let go, as it does on a team with a long history.
	var (
		mu      sync.Mutex
		steps   []searchStep
		started = make(chan bool, 16)
		letGo   = make(chan struct{})
		held    sync.Once
	)
	seen := func() []searchStep {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(steps)
	}
	before := teamInSearch
	t.Cleanup(func() {
		helpers.SettleBackground(time.Minute)
		teamInSearch = before
	})
	teamInSearch = func(teamUUID string, archivedAt *int64, _ *dgraphStruct.DgraphTeam) {
		if teamUUID != launch.String() {
			t.Errorf("search was asked about %s, not Launch", teamUUID)
		}
		archived := archivedAt != nil
		mu.Lock()
		steps = append(steps, searchStep{archived: archived})
		mu.Unlock()
		started <- archived
		if archived {
			held.Do(func() { <-letGo })
		}
		mu.Lock()
		steps = append(steps, searchStep{archived: archived, ended: true})
		mu.Unlock()
	}

	if err := ArchiveTeamByTeamUUID(ctx, launch, info); err != nil {
		t.Fatalf("archiving Launch: %v", err)
	}
	select {
	case archived := <-started:
		if !archived {
			t.Fatal("archiving Launch asked search to bring it back")
		}
	case <-time.After(time.Minute):
		t.Fatal("archiving Launch never asked search to mark it")
	}
	if err := UnArchiveTeamByTeamUUID(ctx, launch, info); err != nil {
		t.Fatalf("restoring Launch: %v", err)
	}
	const grace = 2 * time.Second
	select {
	case <-started:
		t.Error("the restore's clear started while the archive's mark was still running")
	case <-time.After(grace):
	}
	close(letGo)
	for deadline := time.Now().Add(time.Minute); len(seen()) < 4 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	want := []searchStep{{archived: true}, {archived: true, ended: true}, {archived: false}, {archived: false, ended: true}}
	if got := seen(); !slices.Equal(got, want) {
		t.Errorf("archived then restored, search was asked for %+v; want the mark, then the clear after it", got)
	}
}
