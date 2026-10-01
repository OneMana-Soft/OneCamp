//go:build integration

package business

// Custom statuses against a real Postgres 12 (every migration applied) and a
// real Dgraph with the production schema.
// Run: go test -tags=integration ./business/TaskStatus/ -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphTaskModel "github.com/akashc777/OneCamp/models/dgraph/Task"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

type taskRow struct {
	Status     string `json:"task_status"`
	Custom     string `json:"task_custom_status"`
	CustomName string `json:"task_custom_status_name"`
}

func readTask(t *testing.T, id string) taskRow {
	t.Helper()
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(context.Background(),
		`query q($id: string) { t(func: eq(task_uuid, $id)) { task_status task_custom_status task_custom_status_name } }`,
		map[string]string{"$id": id})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ T []taskRow }
	_ = json.Unmarshal(resp.Json, &out)
	if len(out.T) != 1 {
		t.Fatalf("task %s: %d nodes", id, len(out.T))
	}
	return out.T[0]
}

func countTasks(t *testing.T) int {
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(context.Background(), `{ c(func: type(Task)) { n: count(uid) } }`)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ C []struct{ N int } }
	_ = json.Unmarshal(resp.Json, &out)
	if len(out.C) == 0 {
		return 0
	}
	return out.C[0].N
}

func matching(t *testing.T, clause string) map[string]bool {
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(context.Background(),
		fmt.Sprintf(`{ t(func: type(Task)) @filter(%s) { task_uuid } }`, clause))
	if err != nil {
		t.Fatalf("clause %s: %v", clause, err)
	}
	var out struct {
		T []struct {
			ID string `json:"task_uuid"`
		}
	}
	_ = json.Unmarshal(resp.Json, &out)
	m := map[string]bool{}
	for _, x := range out.T {
		m[x.ID] = true
	}
	return m
}

func TestCustomStatuses(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	project, admin := uuid.New(), uuid.New()
	var repoints []Repoint
	OnRepoint(func(_ context.Context, r Repoint) error { repoints = append(repoints, r); return nil })
	lastRepoint := func() Repoint {
		if len(repoints) == 0 {
			return Repoint{}
		}
		return repoints[len(repoints)-1]
	}

	qa, err := Create(ctx, project, admin, Input{Name: "QA", Category: "inReview", Color: "violet"})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := Create(ctx, project, admin, Input{Name: "Blocked", Category: "inProgress"})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("names are unique per project, whatever the case", func(t *testing.T) {
		if _, err := Create(ctx, project, admin, Input{Name: "qa", Category: "todo"}); !errors.Is(err, ErrNameTaken) {
			t.Fatalf("got %v", err)
		}
		if _, err := Create(ctx, uuid.New(), admin, Input{Name: "QA", Category: "todo"}); err != nil {
			t.Fatalf("another project cannot use the name: %v", err)
		}
	})

	t.Run("resolve understands ids, names and built-in labels", func(t *testing.T) {
		for in, want := range map[string]Resolved{
			qa.ID.String(): {Category: "inReview", CustomID: qa.ID.String(), CustomName: "QA"},
			"qa":           {Category: "inReview", CustomID: qa.ID.String(), CustomName: "QA"},
			"In progress":  {Category: "inProgress"},
			"done":         {Category: "done"},
			"":             {Category: "todo"},
		} {
			got, err := Resolve(ctx, project.String(), in)
			if err != nil || got != want {
				t.Errorf("%q -> %+v %v, want %+v", in, got, err, want)
			}
		}
		if _, err := Resolve(ctx, project.String(), "shipping"); !errors.Is(err, ErrUnknownStatus) {
			t.Errorf("unknown name: %v", err)
		}
		if _, err := Resolve(ctx, uuid.New().String(), qa.ID.String()); !errors.Is(err, ErrUnknownStatus) {
			t.Errorf("another project's status was accepted: %v", err)
		}
	})

	// Two tasks in QA, one plain In Review, one Blocked.
	a, b, plain, blk := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	qaID, qaName, blID, blName := qa.ID.String(), "QA", blocked.ID.String(), "Blocked"
	dg.Mutate(t, []map[string]any{
		{"dgraph.type": "Task", "task_uuid": a, "task_status": "inReview", "task_custom_status": qaID, "task_custom_status_name": qaName},
		{"dgraph.type": "Task", "task_uuid": b, "task_status": "inReview", "task_custom_status": qaID, "task_custom_status_name": qaName},
		{"dgraph.type": "Task", "task_uuid": plain, "task_status": "inReview"},
		{"dgraph.type": "Task", "task_uuid": blk, "task_status": "inProgress", "task_custom_status": blID, "task_custom_status_name": blName},
	})

	t.Run("the list filter tells a built-in status from its custom ones", func(t *testing.T) {
		got := matching(t, FilterClause([]string{"inReview"}))
		if !got[plain] || got[a] || got[b] {
			t.Fatalf("In Review matched %v", got)
		}
		got = matching(t, FilterClause([]string{qaID}))
		if !got[a] || !got[b] || got[plain] {
			t.Fatalf("QA matched %v", got)
		}
		got = matching(t, FilterClause([]string{"inReview", blID}))
		if !got[plain] || !got[blk] || got[a] || len(got) != 2 {
			t.Fatalf("In Review or Blocked matched %v", got)
		}
	})

	t.Run("a named status filters as people mean it", func(t *testing.T) {
		// A built-in status is its whole category: In Review includes QA.
		clause, err := QueryClause(ctx, project.String(), "In review")
		if err != nil {
			t.Fatal(err)
		}
		if got := matching(t, clause); !got[a] || !got[b] || !got[plain] || got[blk] {
			t.Fatalf("In review matched %v", got)
		}
		// A project's own, by name, in the project and across projects.
		for _, pid := range []string{project.String(), ""} {
			clause, err := QueryClause(ctx, pid, "qa")
			if err != nil {
				t.Fatalf("project %q: %v", pid, err)
			}
			if got := matching(t, clause); !got[a] || !got[b] || got[plain] || got[blk] {
				t.Fatalf("project %q: QA matched %v", pid, got)
			}
		}
		for _, pid := range []string{project.String(), ""} {
			if _, err := QueryClause(ctx, pid, "Shipped"); !errors.Is(err, ErrUnknownStatus) {
				t.Fatalf("project %q: an unknown name gave %v", pid, err)
			}
		}
	})

	t.Run("a bulk change names the tasks it changed, for search to follow", func(t *testing.T) {
		from := uuid.NewString()
		x, y := uuid.NewString(), uuid.NewString()
		dg.Mutate(t, []map[string]any{
			{"dgraph.type": "Task", "task_uuid": x, "task_status": "todo", "task_custom_status": from, "task_custom_status_name": "X"},
			{"dgraph.type": "Task", "task_uuid": y, "task_status": "todo", "task_custom_status": from, "task_custom_status_name": "X"},
		})
		query := fmt.Sprintf(`query {
			task as var(func: eq(task_custom_status, %q))
			affected(func: uid(task)) { task_uuid }
		}`, from)
		got, err := dgraphTaskModel.UpdateExistingTasksReturning(ctx, &dgraphStruct.DgraphTask{Uid: "uid(task)", Status: "done", DType: []string{"Task"}}, query, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || !((got[0] == x && got[1] == y) || (got[0] == y && got[1] == x)) {
			t.Fatalf("affected %v", got)
		}
		if readTask(t, x).Status != "done" {
			t.Fatal("the change was not made")
		}
		none, err := dgraphTaskModel.UpdateExistingTasksReturning(ctx, &dgraphStruct.DgraphTask{Uid: "uid(task)", Status: "done", DType: []string{"Task"}},
			`query { task as var(func: eq(task_custom_status, "nothing")) affected(func: uid(task)) { task_uuid } }`, "")
		if err != nil || len(none) != 0 {
			t.Fatalf("an empty match: %v %v", none, err)
		}
	})

	t.Run("open work excludes done and canceled, whatever a status is called", func(t *testing.T) {
		done, canceled, dropped := uuid.NewString(), uuid.NewString(), uuid.NewString()
		dg.Mutate(t, []map[string]any{
			{"dgraph.type": "Task", "task_uuid": done, "task_status": "done"},
			{"dgraph.type": "Task", "task_uuid": canceled, "task_status": "canceled"},
			// A project's own status that counts as Canceled.
			{"dgraph.type": "Task", "task_uuid": dropped, "task_status": "canceled", "task_custom_status": uuid.NewString(), "task_custom_status_name": "Dropped"},
		})
		got := matching(t, dgraphStruct.TASK_OPEN_FILTER)
		if got[done] || got[canceled] || got[dropped] {
			t.Fatalf("closed work counted as open: %v", got)
		}
		if !got[a] || !got[plain] || !got[blk] {
			t.Fatalf("open work missing: %v", got)
		}
		if !IsClosed("done") || !IsClosed("canceled") || IsClosed("inReview") {
			t.Fatal("IsClosed disagrees with the filter")
		}
	})

	t.Run("moving a task out of a custom status clears it, and has() agrees", func(t *testing.T) {
		err := taskDomain.UpdateDgraphTaskClearing(ctx, &dgraphStruct.DgraphTask{Uid: "uid(task)", Uuid: blk, Status: "done"},
			[]string{"task_custom_status", "task_custom_status_name"})
		if err != nil {
			t.Fatal(err)
		}
		if got := readTask(t, blk); got != (taskRow{Status: "done"}) {
			t.Fatalf("got %+v", got)
		}
		if matching(t, FilterClause([]string{"done"}))[blk] == false {
			t.Fatal("a task moved out of its custom status is not shown under its built-in one")
		}
	})

	t.Run("renaming and recategorising carries the tasks along", func(t *testing.T) {
		if _, err := Update(ctx, project, qa.ID, Input{Name: "Quality check", Category: "done", Color: "violet"}); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{a, b} {
			if got := readTask(t, id); got != (taskRow{Status: "done", Custom: qaID, CustomName: "Quality check"}) {
				t.Fatalf("%s: %+v", id, got)
			}
		}
		if got := readTask(t, plain); got != (taskRow{Status: "inReview"}) {
			t.Fatalf("an unrelated task changed: %+v", got)
		}
		if r := lastRepoint(); !r.Matches("qa") || r.New != qaID || r.ProjectID != project {
			t.Fatalf("a rename did not point references to the old name at the id: %+v", r)
		}
		n := len(repoints)
		if _, err := Update(ctx, project, qa.ID, Input{Name: "Quality check", Category: "inReview", Color: "violet"}); err != nil {
			t.Fatal(err)
		}
		if len(repoints) != n {
			t.Fatal("a change that kept the name repointed references")
		}
		if _, err := Update(ctx, project, qa.ID, Input{Name: "Quality check", Category: "done", Color: "violet"}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a bulk change over no tasks creates nothing", func(t *testing.T) {
		before := countTasks(t)
		empty, _ := Create(ctx, project, admin, Input{Name: "Empty", Category: "todo"})
		if _, err := Update(ctx, project, empty.ID, Input{Name: "Still empty", Category: "backlog"}); err != nil {
			t.Fatal(err)
		}
		if err := Delete(ctx, project, empty.ID, ""); err != nil {
			t.Fatal(err)
		}
		if after := countTasks(t); after != before {
			t.Fatalf("tasks went from %d to %d", before, after)
		}
	})

	t.Run("deleting moves its tasks to the chosen status, or back to its category", func(t *testing.T) {
		if err := Delete(ctx, project, qa.ID, qa.ID.String()); !errors.Is(err, ErrInvalid) {
			t.Fatalf("moving tasks into the status being deleted: %v", err)
		}
		if err := Delete(ctx, project, qa.ID, "todo"); err != nil {
			t.Fatal(err)
		}
		if r := lastRepoint(); !r.Matches(qaID) || !r.Matches("quality CHECK") || r.New != "todo" {
			t.Fatalf("a delete did not point references where its tasks went: %+v", r)
		}
		for _, id := range []string{a, b} {
			if got := readTask(t, id); got != (taskRow{Status: "todo"}) {
				t.Fatalf("%s: %+v", id, got)
			}
		}
		if _, err := Resolve(ctx, project.String(), qaID); !errors.Is(err, ErrUnknownStatus) {
			t.Fatalf("a deleted status still resolves: %v", err)
		}
		if err := Delete(ctx, project, qa.ID, ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleting twice: %v", err)
		}
	})

	t.Run("reorder and the cap", func(t *testing.T) {
		c1, _ := Create(ctx, project, admin, Input{Name: "C1", Category: "todo"})
		c2, _ := Create(ctx, project, admin, Input{Name: "C2", Category: "todo"})
		order := func() string {
			res, _ := List(ctx, project)
			names := ""
			for _, c := range res.Custom {
				names += c.Name + ","
			}
			return names
		}
		if got := order(); got != "C1,C2,Blocked," {
			t.Fatalf("created order, grouped by category: %s", got)
		}
		if err := Reorder(ctx, project, []string{c2.ID.String(), c1.ID.String(), blocked.ID.String()}); err != nil {
			t.Fatal(err)
		}
		if got := order(); got != "C2,C1,Blocked," {
			t.Fatalf("after reorder: %s", got)
		}
		res, _ := List(ctx, project)
		for i := len(res.Custom); i < MaxPerProject; i++ {
			if _, err := Create(ctx, project, admin, Input{Name: fmt.Sprintf("S%d", i), Category: "todo"}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := Create(ctx, project, admin, Input{Name: "One too many", Category: "todo"}); !errors.Is(err, ErrTooMany) {
			t.Fatalf("got %v", err)
		}
	})
}
