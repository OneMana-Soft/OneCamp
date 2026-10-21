//go:build integration

package models_test

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	nudges "github.com/akashc777/OneCamp/models/postgres/WorkspaceNudge"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// Backend audit #36: the hourly nudge pass checked and wrote each of a
// person's nudges alone. They are one read and one write, and a write says
// which nudges are new (each published) and which it refreshed.
func TestAPersonsNudgesAreWrittenInOneStatement(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	me := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, 'me@example.test')`, me); err != nil {
		t.Fatal(err)
	}
	in := []nudges.UpsertInput{
		{Kind: nudges.KindOverdueCommitment, Title: "Ship it", SourceType: "task", SourceID: "t1", Priority: 3, DedupKey: "k1"},
		{Kind: nudges.KindOverdueCommitment, Title: "Review", CTAURL: "/app/t2", SourceType: "task", SourceID: "t2", Priority: 2, DedupKey: "k2"},
		{Kind: nudges.KindOverdueCommitment, Title: "Again", SourceType: "task", SourceID: "t1", Priority: 1, DedupKey: "k1"},
	}
	got, err := nudges.UpsertMany(ctx, me, in)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]nudges.Upserted{}
	for _, u := range got {
		byKey[u.DedupKey] = u
	}
	if len(got) != 2 || !byKey["k1"].Inserted || !byKey["k2"].Inserted {
		t.Fatalf("first pass: %+v", got)
	}
	in[0].Title = "Ship it today"
	again, err := nudges.UpsertMany(ctx, me, in[:1])
	if err != nil || len(again) != 1 || again[0].Inserted || again[0].ID != byKey["k1"].ID {
		t.Fatalf("second pass: %+v %v (first %+v)", again, err, byKey["k1"])
	}
	var title string
	var cta *string
	if err := env.PG.QueryRow(`SELECT title, cta_url FROM workspace_nudges WHERE dedup_key = 'k1'`).Scan(&title, &cta); err != nil || title != "Ship it today" || cta != nil {
		t.Errorf("k1 now: %q %v %v", title, cta, err)
	}
	if _, err := env.PG.Exec(`UPDATE workspace_nudges SET status = 'dismissed' WHERE dedup_key = 'k2'`); err != nil {
		t.Fatal(err)
	}
	handled, err := nudges.RecentlyTerminal(ctx, []string{"k1", "k2", "k3"}, time.Hour)
	if err != nil || !handled["k2"] || handled["k1"] || handled["k3"] {
		t.Errorf("recently handled: %v %v", handled, err)
	}
}
