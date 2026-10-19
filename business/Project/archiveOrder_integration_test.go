//go:build integration

package business

// A project's search follows its archive in the order it was archived and
// restored, against a real Postgres and Dgraph.
//
// Archiving a project marks it, and its tasks, comments, files and AI
// embeddings, deleted in search; restoring it clears the marks. Each started
// on goroutines of their own, so a restore whose clear finished before the
// archive's mark left a live project's whole content out of search. Modelled
// on the channels' (c9c26b3a).
//
// Run: go test -tags=integration ./business/Project/ -run TestAProjectsSearchFollowsItsArchiveInOrder -v

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

func TestAProjectsSearchFollowsItsArchiveInOrder(t *testing.T) {
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
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Spring launch', $2)`, launch, ana); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": ana.String(), "user_name": "ana"},
		{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": launch.String(), "project_name": "Spring launch",
			"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": []map[string]any{{"uid": "_:ana"}},
			"project_admins": []map[string]any{{"uid": "_:ana"}}},
	})
	info := &dgraphStruct.DgraphProject{Uuid: launch.String(), Name: "Spring launch"}

	// Every change asked of search, as it starts and as it ends. The archive's
	// holds until let go, as it does on a project with a long history.
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
	before := projectInSearch
	t.Cleanup(func() {
		helpers.SettleBackground(time.Minute)
		projectInSearch = before
	})
	projectInSearch = func(projectUUID string, archivedAt *int64, _ *dgraphStruct.DgraphProject) {
		if projectUUID != launch.String() {
			t.Errorf("search was asked about %s, not Spring launch", projectUUID)
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

	if err := ArchiveProjectByProjectUUID(ctx, launch, info); err != nil {
		t.Fatalf("archiving Spring launch: %v", err)
	}
	select {
	case archived := <-started:
		if !archived {
			t.Fatal("archiving Spring launch asked search to bring it back")
		}
	case <-time.After(time.Minute):
		t.Fatal("archiving Spring launch never asked search to mark it")
	}
	if err := UnArchiveProjectUUID(ctx, launch, info); err != nil {
		t.Fatalf("restoring Spring launch: %v", err)
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
