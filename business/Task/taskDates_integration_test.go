//go:build integration

package business

// A task's dates against a real Postgres and Dgraph with the production schema.
// Run: go test -tags=integration ./business/Task/ -run TestUpdateTaskDates -v

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

func TestUpdateTaskDates(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	id := uuid.New()
	unset := "0001-01-01T00:00:00Z"
	dg.Mutate(t, map[string]any{"dgraph.type": "Task", "task_uuid": id.String(), "task_name": "Plan",
		"task_start_date": unset, "task_due_date": "2026-03-04T17:00:00Z", "task_deleted_at": unset})
	user := &dgraphStruct.DgraphUser{Uid: dg.Mutate(t, map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": uuid.NewString()})["u"]}

	read := func() (start, due time.Time, lines int) {
		t.Helper()
		resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx,
			`query q($id: string) { t(func: eq(task_uuid, $id)) { task_start_date task_due_date n: count(task_activities) } }`,
			map[string]string{"$id": id.String()})
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			T []struct {
				Start time.Time `json:"task_start_date"`
				Due   time.Time `json:"task_due_date"`
				N     int       `json:"n"`
			}
		}
		if err := json.Unmarshal(resp.Json, &out); err != nil || len(out.T) != 1 {
			t.Fatalf("read back: %v %s", err, resp.Json)
		}
		return out.T[0].Start, out.T[0].Due, out.T[0].N
	}
	day := func(s string) *time.Time {
		v, _ := time.Parse(time.RFC3339, s)
		return &v
	}
	zero := time.Time{}

	start, due := day("2026-03-02T09:00:00Z"), day("2026-03-06T17:00:00Z")
	if err := UpdateTaskDates(ctx, id, TaskDates{Start: start, Due: due}, &dgraphStruct.DgraphTask{StartDate: &zero, DueDate: day("2026-03-04T17:00:00Z")}, user); err != nil {
		t.Fatal(err)
	}
	if s, d, n := read(); !s.Equal(*start) || !d.Equal(*due) || n != 2 {
		t.Fatalf("both dates in one write, a history line for each: %v %v %d", s, d, n)
	}

	if err := UpdateTaskDates(ctx, id, TaskDates{Start: start, Due: due}, &dgraphStruct.DgraphTask{StartDate: start, DueDate: due}, user); err != nil {
		t.Fatal(err)
	}
	if _, _, n := read(); n != 2 {
		t.Fatalf("dates already so write nothing: %d lines", n)
	}

	later := day("2026-03-09T17:00:00Z")
	if err := UpdateTaskDates(ctx, id, TaskDates{Start: start, Due: later}, &dgraphStruct.DgraphTask{StartDate: start, DueDate: due}, user); err != nil {
		t.Fatal(err)
	}
	if s, d, n := read(); !s.Equal(*start) || !d.Equal(*later) || n != 3 {
		t.Fatalf("only the date that changed gets a line: %v %v %d", s, d, n)
	}

	if err := UpdateTaskDates(ctx, id, TaskDates{Start: &zero}, &dgraphStruct.DgraphTask{StartDate: start, DueDate: later}, user); err != nil {
		t.Fatal(err)
	}
	if s, d, _ := read(); s.Year() > 1970 || !d.Equal(*later) {
		t.Fatalf("the zero time clears a date and leaves the other: %v %v", s, d)
	}

	// Deleted by someone else while a timeline still shows it: refused, and
	// the task stays deleted (a write used to set task_deleted_at back to zero).
	gone := day("2026-03-10T08:00:00Z")
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx,
		`query q($id: string) { t(func: eq(task_uuid, $id)) { uid } }`, map[string]string{"$id": id.String()})
	var found struct{ T []struct{ UID string } }
	if err != nil || json.Unmarshal(resp.Json, &found) != nil || len(found.T) != 1 {
		t.Fatalf("finding the task: %v", err)
	}
	dg.Mutate(t, map[string]any{"uid": found.T[0].UID, "task_deleted_at": gone.Format(time.RFC3339)})
	info, err := GetDgraphBasicTaskInfo(ctx, id.String(), user.Uid)
	if err != nil || info == nil || info.DeletedAt == nil || !info.DeletedAt.Equal(*gone) {
		t.Fatalf("the task's basic info must say when it was deleted: %+v %v", info, err)
	}
	if err := UpdateTaskDates(ctx, id, TaskDates{Due: day("2026-03-12T17:00:00Z")}, info, user); !errors.Is(err, ErrTaskDeleted) {
		t.Fatalf("a deleted task's dates were changed: %v", err)
	}
	if _, d, _ := read(); !d.Equal(*later) {
		t.Fatalf("a refused change wrote a date: %v", d)
	}
}
