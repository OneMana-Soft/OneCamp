//go:build integration

package business

// Presence on a table (tables A7), against Postgres 12 and Redis: someone
// opening a table is there for its readers, others hear when they move but not
// when they only say they're still there, someone silent for 45 s is gone, and
// a reader who can't open the table can't say they're on it.
// Run: integ_rel.sh <worktree> <log> '^(TestPresenceOnATable)$' with PKGS='./business/DataTable/'

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestPresenceOnATable(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.SetupRedis(t)
	person := func(name string) uuid.UUID {
		id := uuid.New()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		return id
	}
	maya, sam, ben := Actor{UserID: person("maya")}, Actor{UserID: person("sam")}, Actor{UserID: person("ben")}
	shared, err := CreateTable(ctx, TableInput{Name: "Deals", Visibility: "workspace"}, maya)
	if err != nil {
		t.Fatal(err)
	}
	private, err := CreateTable(ctx, TableInput{Name: "Salaries", Visibility: "private"}, maya)
	if err != nil {
		t.Fatal(err)
	}
	var told [][]Here
	tell := tellPresence
	tellPresence = func(_ string, people []Here) { told = append(told, people) }
	t.Cleanup(func() { tellPresence = tell })

	row, field := uuid.NewString(), uuid.NewString()
	now := time.Now()
	people, err := SetPresence(ctx, shared.Id, maya, Here{Row: row, Field: field, Editing: true}, false, now)
	if err != nil || len(people) != 1 || people[0].User != maya.UserID.String() || !people[0].Editing {
		t.Fatalf("Maya opens it, editing a cell: %+v %v", people, err)
	}
	if people, _ = SetPresence(ctx, shared.Id, sam, Here{}, false, now.Add(time.Second)); len(people) != 2 {
		t.Errorf("Sam sees Maya too: %+v", people)
	}
	for _, h := range people {
		if h.Name != "maya" && h.Name != "sam" {
			t.Errorf("each named as the table names people: %+v", h)
		}
	}
	if len(told) != 2 {
		t.Errorf("each coming is told: %d", len(told))
	}
	// A heartbeat at the same place is not told.
	if _, err := SetPresence(ctx, shared.Id, maya, Here{Row: row, Field: field, Editing: true}, false, now.Add(20*time.Second)); err != nil || len(told) != 2 {
		t.Errorf("a heartbeat was told: %d %v", len(told), err)
	}
	// Sam has been silent 50 s when Maya next says she's there: he's gone, and that's told.
	people, _ = SetPresence(ctx, shared.Id, maya, Here{Row: row, Field: field}, false, now.Add(51*time.Second))
	if len(people) != 1 || len(told) != 3 {
		t.Errorf("Sam, silent, has gone: %+v (told %d)", people, len(told))
	}
	// Maya leaves.
	if people, _ = SetPresence(ctx, shared.Id, maya, Here{}, true, now.Add(52*time.Second)); len(people) != 0 || len(told) != 4 {
		t.Errorf("Maya left: %+v (told %d)", people, len(told))
	}
	// Ben can't open Maya's private table, so he can't be on it.
	if _, err := SetPresence(ctx, private.Id, ben, Here{}, false, now); err == nil {
		t.Error("someone who can't open a table said they were on it")
	}
}
