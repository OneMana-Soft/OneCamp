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
		{UserID: me, Kind: nudges.KindOverdueCommitment, Title: "Ship it", SourceType: "task", SourceID: "t1", Priority: 3, DedupKey: "k1"},
		{UserID: me, Kind: nudges.KindOverdueCommitment, Title: "Review", CTAURL: "/app/t2", SourceType: "task", SourceID: "t2", Priority: 2, DedupKey: "k2"},
		{UserID: me, Kind: nudges.KindOverdueCommitment, Title: "Again", SourceType: "task", SourceID: "t1", Priority: 1, DedupKey: "k1"},
	}
	got, err := nudges.UpsertMany(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]nudges.Upserted{}
	for _, u := range got {
		byKey[u.DedupKey] = u
	}
	if len(got) != 2 || !byKey["k1"].Inserted || !byKey["k2"].Inserted || byKey["k1"].UserID != me || byKey["k2"].UserID != me {
		t.Fatalf("first pass: %+v", got)
	}
	in[0].Title = "Ship it today"
	again, err := nudges.UpsertMany(ctx, in[:1])
	if err != nil || len(again) != 1 || again[0].Inserted || again[0].ID != byKey["k1"].ID {
		t.Fatalf("second pass: %+v %v (first %+v)", again, err, byKey["k1"])
	}
	var title string
	var cta *string
	if err := env.PG.QueryRow(`SELECT title, cta_url FROM workspace_nudges WHERE dedup_key = 'k1'`).Scan(&title, &cta); err != nil || title != "Ship it today" || cta != nil {
		t.Errorf("k1 now: %q %v %v", title, cta, err)
	}
	if _, err := env.PG.Exec(`UPDATE workspace_nudges SET status = 'dismissed', updated_at = NOW() - interval '2 days' WHERE dedup_key = 'k2'`); err != nil {
		t.Fatal(err)
	}
	// Another member's dismissal of k3 is theirs: it never holds mine.
	other := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, 'other@example.test')`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO workspace_nudges (user_id, kind, title, dedup_key, status)
		VALUES ($1, 'overdue_commitment', 'Theirs', 'k3', 'dismissed')`, other); err != nil {
		t.Fatal(err)
	}
	// Both are read in one statement, each for their own keys.
	handled, err := nudges.LastHandled(ctx, map[uuid.UUID][]string{me: {"k1", "k2", "k3"}, other: {"k2", "k3"}})
	if at, ok := handled[me]["k2"]; err != nil || len(handled[me]) != 1 || !ok || time.Since(at) < 47*time.Hour || time.Since(at) > 49*time.Hour {
		t.Errorf("my last handled: %v %v; want only k2, two days ago", handled[me], err)
	}
	if _, ok := handled[other]["k3"]; !ok || len(handled[other]) != 1 {
		t.Errorf("their last handled: %v; want only their own k3, not my k2", handled[other])
	}
}
