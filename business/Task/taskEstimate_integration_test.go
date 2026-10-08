//go:build integration

package business

// A task's estimate against a real Postgres and Dgraph with the production schema.
// Run: go test -tags=integration ./business/Task/ -run TestUpdateTaskEstimate -v

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestUpdateTaskEstimate(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	id := uuid.New()
	live := "0001-01-01T00:00:00Z"
	dg.Mutate(t, map[string]any{"dgraph.type": "Task", "task_uuid": id.String(), "task_name": "Plan", "task_deleted_at": live})
	user := &dgraphStruct.DgraphUser{Uid: dg.Mutate(t, map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": uuid.NewString()})["u"]}

	read := func() (minutes, lines int) {
		t.Helper()
		resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx,
			`query q($id: string) { t(func: eq(task_uuid, $id)) { task_estimate_minutes n: count(task_activities) } }`,
			map[string]string{"$id": id.String()})
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			T []struct {
				Minutes int `json:"task_estimate_minutes"`
				N       int `json:"n"`
			}
		}
		if err := json.Unmarshal(resp.Json, &out); err != nil || len(out.T) != 1 {
			t.Fatalf("read back: %v %s", err, resp.Json)
		}
		return out.T[0].Minutes, out.T[0].N
	}
	task := func() *dgraphStruct.DgraphTask {
		t.Helper()
		got, err := GetDgraphBasicTaskInfo(ctx, id.String(), user.Uid)
		if err != nil || got == nil {
			t.Fatalf("basic info: %v", err)
		}
		return got
	}

	if err := UpdateTaskEstimate(ctx, id, 90, task(), user); err != nil {
		t.Fatal(err)
	}
	if m, n := read(); m != 90 || n != 1 {
		t.Fatalf("an hour and a half, with a line in the history: %d %d", m, n)
	}
	if got := task(); got.EstimateMinutes == nil || *got.EstimateMinutes != 90 {
		t.Fatal("the task's basic info carries the estimate, so the next change knows what it was")
	}
	if err := UpdateTaskEstimate(ctx, id, 90, task(), user); err != nil {
		t.Fatal(err)
	}
	if _, n := read(); n != 1 {
		t.Fatalf("the same estimate again writes nothing: %d lines", n)
	}
	for _, bad := range []int{-1, MaxEstimateMinutes + 1} {
		if err := UpdateTaskEstimate(ctx, id, bad, task(), user); !errors.Is(err, ErrEstimateRange) {
			t.Fatalf("%d minutes: %v", bad, err)
		}
	}
	if err := UpdateTaskEstimate(ctx, id, 0, task(), user); err != nil {
		t.Fatal(err)
	}
	if m, n := read(); m != 0 || n != 2 {
		t.Fatalf("taken off, and said so: %d %d", m, n)
	}
	gone := task()
	deleted := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	gone.DeletedAt = &deleted
	if err := UpdateTaskEstimate(ctx, id, 30, gone, user); !errors.Is(err, ErrTaskDeleted) {
		t.Fatalf("a deleted task's estimate can't be changed: %v", err)
	}
}
