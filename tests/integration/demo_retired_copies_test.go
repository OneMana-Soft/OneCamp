//go:build integration
// +build integration

package integration_test

// Activity on the demo listed every seeded mention twice. Each nightly
// refresh retires the curated channels (renamed "<name>-retired-<time>" and
// archived) and is meant to take the visitor out of every retired copy, since
// Activity lists mentions from the channels the reader is in. The lookup of
// retired copies read live channels only, and retiring archives a channel, so
// it never found one: the visitor stayed in each night's copy and saw its
// mentions again.
//
// The curated projects are retired the same way, and a rename wrote the
// project's whole search entry back to live, so a visitor searching "launch"
// found "Q4 launch-retired-1791614951". Each refresh now takes every archived
// copy out of search again, found by the lookup tested below.
//
// Run: go test -tags=integration ./tests/integration/ -run TestRetiredDemo -v

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	channelModels "github.com/akashc777/OneCamp/models/postgres/Channel"
	projectModels "github.com/akashc777/OneCamp/models/postgres/Project"
)

func TestRetiredDemoCopiesAreFoundThoughArchived(t *testing.T) {
	wireStores(t)
	ctx := context.Background()
	pg := postgresInit.DBConn.SqlDB

	archived, live, other := uuid.New(), uuid.New(), uuid.New()
	for _, row := range []struct {
		id       uuid.UUID
		name     string
		archived bool
	}{
		{archived, "design-retired-1791614952", true},
		{live, "design-retired-1791700000", false},
		{other, "designers", false},
	} {
		q := `INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, false)`
		if row.archived {
			q = `INSERT INTO channels (id, ch_name, ch_private, deleted_at) VALUES ($1, $2, false, now())`
		}
		if _, err := pg.ExecContext(ctx, q, row.id, row.name); err != nil {
			t.Fatalf("channel %s: %v", row.name, err)
		}
	}

	all, err := channelModels.ChannelIDsWithNamePrefix(ctx, "design-retired-", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || !slices.Contains(all, archived) || !slices.Contains(all, live) {
		t.Errorf("retired copies, archived too: got %v, want %v and %v", all, archived, live)
	}

	liveOnly, err := channelModels.ChannelIDsWithNamePrefix(ctx, "design-retired-", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(liveOnly) != 1 || liveOnly[0] != live {
		t.Errorf("live channels only: got %v, want [%v]", liveOnly, live)
	}
}

func TestRetiredDemoProjectsAreFoundWhenArchived(t *testing.T) {
	wireStores(t)
	ctx := context.Background()
	pg := postgresInit.DBConn.SqlDB

	owner := uuid.New()
	if _, err := pg.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'owner@example.com', 'owner-retired', 'Owner', now(), now())`, owner); err != nil {
		t.Fatalf("owner: %v", err)
	}
	archived, live, other := uuid.New(), uuid.New(), uuid.New()
	for _, row := range []struct {
		id       uuid.UUID
		name     string
		archived bool
	}{
		{archived, "Q4 launch-retired-1791614951", true},
		{live, "Q4 launch-retired-1791700000", false}, // being retired: not yet archived
		{other, "Q4 launch", false},
	} {
		q := `INSERT INTO projects (id, project_name, created_by) VALUES ($1, $2, $3)`
		if row.archived {
			q = `INSERT INTO projects (id, project_name, created_by, deleted_at) VALUES ($1, $2, $3, now())`
		}
		if _, err := pg.ExecContext(ctx, q, row.id, row.name, owner); err != nil {
			t.Fatalf("project %s: %v", row.name, err)
		}
	}

	got, err := projectModels.ArchivedProjectsWithNamePrefix(ctx, "Q4 launch-retired-")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Id != archived || got[0].ArchivedAt.IsZero() {
		t.Errorf("archived retired copies: got %+v, want only %v with its archive time", got, archived)
	}
}
