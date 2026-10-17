//go:build integration
// +build integration

package integration_test

// The demo's project updates are written as of the days before the visitor
// arrives (business/DemoSeed, PTC-17): an update made with AddAt keeps the
// time it was given, and the project's latest update is the newest of them,
// whatever order they were written in.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnUpdateWrittenAtATimeKeepsIt -v

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	updateModel "github.com/akashc777/OneCamp/models/postgres/ProjectUpdate"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnUpdateWrittenAtATimeKeepsIt(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	project, maya := uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	recent := now.AddDate(0, 0, -2)
	older := now.AddDate(0, 0, -9)

	// The newer one first: the latest is by when it was written, not by when it was stored.
	if _, err := updateModel.AddAt(project, maya, updateModel.OnTrack, "On track for launch day.", false, recent); err != nil {
		t.Fatal(err)
	}
	first, err := updateModel.AddAt(project, maya, updateModel.AtRisk, "Scope is agreed.", false, older)
	if err != nil {
		t.Fatal(err)
	}
	if !first.CreatedAt.Equal(older) || !first.UpdatedAt.Equal(older) {
		t.Fatalf("written at %v, kept as %v (updated %v)", older, first.CreatedAt, first.UpdatedAt)
	}
	latest, err := updateModel.Latest([]uuid.UUID{project})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := latest[project]
	if !ok || got.Health != updateModel.OnTrack || !got.CreatedAt.Equal(recent) {
		t.Fatalf("latest = %+v (found %v), want the on-track one from two days ago", got, ok)
	}
	list, err := updateModel.List(project, updateModel.MaxList, false)
	if err != nil || len(list) != 2 || !list[0].CreatedAt.Equal(recent) {
		t.Fatalf("list = %+v, %v; want both, newest first", list, err)
	}
}
