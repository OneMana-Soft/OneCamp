//go:build integration

package integration_test

// jira-04 head C: parallel cycles, opted into per project by its admins. On,
// two cycles that share days both run and every reader lists them both; off
// (the default), an overlap is refused as before; a member can't turn it on;
// turning it off while two open cycles share days is refused in words naming
// them; a schedule (one cycle after another) and parallel cycles exclude each
// other; a task is still in one cycle at most; completing one carries its
// open tasks past the other running one; an upcoming cycle starts, and a
// deleted one comes back, beside a running one that shares its days (off, the
// restore is refused as before); an archived project's cycles don't change,
// restore included, and its admins read them as read-only.
// Run: PKGS=./tests/integration/ integ_rel.sh <wt> <log> '^TestJ04C'

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	restoreController "github.com/akashc777/OneCamp/controllers/Restore"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestJ04CParallelCyclesRunSideBySideForTheProjectsAdmins(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.addTasks(1)
	now := time.Now().UTC()
	listed := func(who string) (parallel bool, states map[string]string) {
		t.Helper()
		code, out := s.read(who, cycleController.ListCycles, "")
		if code != http.StatusOK {
			t.Fatalf("cycles as %s: %d %s", who, code, out)
		}
		var got struct {
			Data struct {
				Parallel bool `json:"parallel"`
				Cycles   []struct {
					Name  string `json:"name"`
					State string `json:"state"`
				} `json:"cycles"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		states = map[string]string{}
		for _, c := range got.Data.Cycles {
			states[c.Name] = c.State
		}
		return got.Data.Parallel, states
	}
	setParallel := func(who string, on bool) (int, string) {
		t.Helper()
		return s.postAs(who, cycleController.SetParallelCycles, map[string]any{"on": on})
	}
	create := func(name string, startsAt time.Time, weeks int) (int, string) {
		t.Helper()
		return s.postAs("ana", cycleController.CreateCycle, map[string]any{"name": name, "starts_at": startsAt, "weeks": weeks})
	}
	cycleNamed := func(name string) cycleModel.Cycle {
		t.Helper()
		all, err := cycleModel.List(mustUUID(t, s.project))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range all {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("no cycle named %s", name)
		return cycleModel.Cycle{}
	}
	restore := func(id string) (int, string) {
		t.Helper()
		return s.call("ana", http.MethodPost, "/restore/cycle/"+id, restoreController.Restore, map[string]string{"kind": "cycle", "id": id}, nil)
	}
	deleted := func(id string) bool {
		t.Helper()
		var gone bool
		if err := env.PG.QueryRow(`SELECT deleted_at IS NOT NULL FROM project_cycles WHERE id = $1`, id).Scan(&gone); err != nil {
			t.Fatal(err)
		}
		return gone
	}

	if on, _ := listed("ben"); on {
		t.Fatal("parallel cycles default to off")
	}
	if code, out := create("Web", now.AddDate(0, 0, -3), 2); code != http.StatusOK {
		t.Fatalf("Ana makes Web: %d %s", code, out)
	}
	if code, out := create("App", now.AddDate(0, 0, -1), 1); code != http.StatusBadRequest || !strings.Contains(out, "overlaps") {
		t.Fatalf("off, a cycle sharing Web's days is refused: %d %s", code, out)
	}
	if code, out := setParallel("ben", true); code != http.StatusForbidden {
		t.Fatalf("Ben, a member, turns parallel cycles on: %d %s, want 403", code, out)
	}
	if on, _ := listed("ben"); on {
		t.Fatal("Ben's refused turn changed the setting")
	}
	if code, out := setParallel("ana", true); code != http.StatusOK {
		t.Fatalf("Ana, an admin, turns parallel cycles on: %d %s", code, out)
	}
	if code, out := create("App", now.AddDate(0, 0, -1), 1); code != http.StatusOK {
		t.Fatalf("on, App shares Web's days: %d %s", code, out)
	}
	web, app := cycleNamed("Web"), cycleNamed("App")

	t.Run("the switcher's read: the setting and both cycles current, for a member", func(t *testing.T) {
		on, states := listed("ben")
		if !on || states["Web"] != "current" || states["App"] != "current" {
			t.Fatalf("parallel %v, states %v", on, states)
		}
	})
	t.Run("cycles across projects list both", func(t *testing.T) {
		code, out := s.call("ben", http.MethodGet, "/", cycleController.RunningCycles, nil, nil)
		if code != http.StatusOK || !strings.Contains(out, web.Id.String()) || !strings.Contains(out, app.Id.String()) {
			t.Fatalf("running cycles: %d %s", code, out)
		}
	})
	t.Run("the backlog plans both", func(t *testing.T) {
		found := map[string]string{}
		for _, c := range b3Backlog(t, s, "ben").Data.Backlog.Cycles {
			found[c.ID] = c.State
		}
		if found[web.Id.String()] != "current" || found[app.Id.String()] != "current" {
			t.Fatalf("backlog cycles %v", found)
		}
		code, out := b3Call(s, "ben", http.MethodGet, cycleController.Backlog, "")
		var got struct {
			Data struct {
				Parallel bool `json:"parallel"`
			} `json:"data"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(out), &got) != nil || !got.Data.Parallel {
			t.Fatalf("the backlog says the cycles run side by side: %d %s", code, out)
		}
	})
	t.Run("a task is in one cycle at most, even with two running", func(t *testing.T) {
		task := s.tasks[0]
		for _, c := range []cycleModel.Cycle{web, app} {
			if code, out := s.call("ana", http.MethodPost, "/", cycleController.SetTaskCycle, nil, []byte(`{"task_uuid":"`+task+`","cycle_id":"`+c.Id.String()+`"}`)); code != http.StatusOK {
				t.Fatalf("put in %s: %d %s", c.Name, code, out)
			}
		}
		var n int
		var in string
		if err := env.PG.QueryRow(`SELECT COUNT(*), MAX(cycle_id::text) FROM task_cycles WHERE task_uuid = $1`, task).Scan(&n, &in); err != nil || n != 1 || in != app.Id.String() {
			t.Fatalf("the task's cycles: %d %s %v, want App alone", n, in, err)
		}
		if _, err := env.PG.Exec(`INSERT INTO task_cycles (task_uuid, cycle_id) VALUES ($1, $2)`, task, web.Id); err == nil {
			t.Fatal("a second cycle for the task was stored")
		}
	})
	t.Run("off while two open cycles share days is refused, naming them", func(t *testing.T) {
		code, out := setParallel("ana", false)
		if code != http.StatusBadRequest || !strings.Contains(out, "Web") || !strings.Contains(out, "App") {
			t.Fatalf("off: %d %s", code, out)
		}
		if on, _ := listed("ben"); !on {
			t.Fatal("the refused turn changed the setting")
		}
	})
	t.Run("a schedule is refused while cycles run side by side", func(t *testing.T) {
		code, out := s.postAs("ana", cycleController.SetCycleSchedule, map[string]any{"weeks": 2, "upcoming": 2, "starts_at": now})
		if code != http.StatusBadRequest || !strings.Contains(out, "parallel") {
			t.Fatalf("schedule: %d %s", code, out)
		}
	})
	// F1: the carry target is the first open cycle after the one completed
	// that shares no days with it (or a new one), never the other team's
	// running cycle.
	t.Run("completing a running cycle carries its open tasks past the other running one", func(t *testing.T) {
		if code, out := create("Next", web.EndsAt.AddDate(0, 0, 1), 1); code != http.StatusOK {
			t.Fatalf("Next, after Web: %d %s", code, out)
		}
		task := s.tasks[0]
		if code, out := s.call("ana", http.MethodPost, "/", cycleController.SetTaskCycle, nil, []byte(`{"task_uuid":"`+task+`","cycle_id":"`+web.Id.String()+`"}`)); code != http.StatusOK {
			t.Fatalf("put in Web: %d %s", code, out)
		}
		code, out := b3Do(s, "ana", http.MethodPost, cycleController.CompleteCycle, web.Id.String(), map[string]any{"carry": true})
		var done struct {
			Data struct {
				Carried int `json:"carried"`
				Next    struct {
					Name string `json:"name"`
				} `json:"next"`
			} `json:"data"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(out), &done) != nil || done.Data.Carried != 1 || done.Data.Next.Name != "Next" {
			t.Fatalf("complete Web, carrying its open task to Next: %d %s", code, out)
		}
		var in string
		if err := env.PG.QueryRow(`SELECT cycle_id::text FROM task_cycles WHERE task_uuid = $1`, task).Scan(&in); err != nil || in != cycleNamed("Next").Id.String() {
			t.Fatalf("Web's open task went to %s (App is %s), want Next: %v", in, app.Id, err)
		}
	})
	t.Run("an upcoming cycle starts while another runs: both run, and a completed one keeps its days", func(t *testing.T) {
		ends := cycleNamed("Web").EndsAt
		if code, out := b3Call(s, "ana", http.MethodPost, cycleController.StartCycle, cycleNamed("Next").Id.String()); code != http.StatusOK {
			t.Fatalf("start Next while App runs: %d %s", code, out)
		}
		if _, states := listed("ben"); states["App"] != "current" || states["Next"] != "current" {
			t.Fatalf("states %v, want App and Next current", states)
		}
		if got := cycleNamed("Web").EndsAt; !got.Equal(ends) {
			t.Fatalf("Web, completed, now ends %v, not %v: side by side, a start ends no other cycle", got, ends)
		}
	})
	t.Run("a deleted cycle that shares days with a running one comes back", func(t *testing.T) {
		next := cycleNamed("Next").Id.String()
		if code, out := b3Call(s, "ana", http.MethodPost, cycleController.DeleteCycle, next); code != http.StatusOK {
			t.Fatalf("delete Next: %d %s", code, out)
		}
		if code, out := restore(next); code != http.StatusOK || deleted(next) {
			t.Fatalf("restore Next beside App: %d %s", code, out)
		}
		if _, states := listed("ben"); states["App"] != "current" || states["Next"] != "current" {
			t.Fatalf("states %v, want App and Next current", states)
		}
	})
	t.Run("a deleted completed cycle comes back with all its days, though App and Next start inside them", func(t *testing.T) {
		done := cycleNamed("Web")
		if code, out := b3Call(s, "ana", http.MethodPost, cycleController.DeleteCycle, done.Id.String()); code != http.StatusOK {
			t.Fatalf("delete Web: %d %s", code, out)
		}
		if code, out := restore(done.Id.String()); code != http.StatusOK || deleted(done.Id.String()) {
			t.Fatalf("restore Web beside App and Next: %d %s", code, out)
		}
		if got := cycleNamed("Web").EndsAt; !got.Equal(done.EndsAt) {
			t.Fatalf("Web came back ending %v, not %v: side by side, a restore ends it where no other cycle starts", got, done.EndsAt)
		}
	})
	t.Run("off once one of them is complete; overlaps are refused again", func(t *testing.T) {
		if _, err := cycleModel.Complete(app.Id, 0, nil, nil); err != nil {
			t.Fatal(err)
		}
		if code, out := setParallel("ana", false); code != http.StatusOK {
			t.Fatalf("off: %d %s", code, out)
		}
		if code, out := create("Ops", now.AddDate(0, 0, 1), 1); code != http.StatusBadRequest {
			t.Fatalf("off again, Ops shares Web's days: %d %s", code, out)
		}
	})
	t.Run("off, a deleted cycle that shares days with another stays deleted, as before", func(t *testing.T) {
		next := cycleNamed("Next").Id.String()
		if code, out := b3Call(s, "ana", http.MethodPost, cycleController.DeleteCycle, next); code != http.StatusOK {
			t.Fatalf("delete Next: %d %s", code, out)
		}
		if code, out := restore(next); code != http.StatusBadRequest || !strings.Contains(out, "can't overlap") || !deleted(next) {
			t.Fatalf("off, restore Next over Web's days: %d %s", code, out)
		}
	})
	t.Run("on is refused while a schedule runs the cycles", func(t *testing.T) {
		s.exec(`INSERT INTO cycle_schedules (project_uuid, weeks, anchor) VALUES ($1, 2, NOW())`, s.project)
		if code, out := setParallel("ana", true); code != http.StatusBadRequest || !strings.Contains(out, "schedule") {
			t.Fatalf("on with a schedule: %d %s", code, out)
		}
		s.exec(`DELETE FROM cycle_schedules WHERE project_uuid = $1`, s.project)
	})
	// The carry rule's "open": a later cycle completed ahead of time is never
	// the target; the first open one after it is.
	t.Run("completing a cycle carries its open tasks past a later one already complete", func(t *testing.T) {
		if code, out := create("Cur", now.AddDate(0, 0, 20), 1); code != http.StatusOK {
			t.Fatalf("Cur: %d %s", code, out)
		}
		cur := cycleNamed("Cur")
		if code, out := create("Later", cur.EndsAt, 1); code != http.StatusOK {
			t.Fatalf("Later: %d %s", code, out)
		}
		if code, out := create("Third", cycleNamed("Later").EndsAt, 1); code != http.StatusOK {
			t.Fatalf("Third: %d %s", code, out)
		}
		if code, out := b3Do(s, "ana", http.MethodPost, cycleController.CompleteCycle, cycleNamed("Later").Id.String(), map[string]any{"carry": false}); code != http.StatusOK {
			t.Fatalf("complete Later ahead of time: %d %s", code, out)
		}
		task := s.tasks[0]
		if code, out := s.call("ana", http.MethodPost, "/", cycleController.SetTaskCycle, nil, []byte(`{"task_uuid":"`+task+`","cycle_id":"`+cur.Id.String()+`"}`)); code != http.StatusOK {
			t.Fatalf("put in Cur: %d %s", code, out)
		}
		if code, out := b3Do(s, "ana", http.MethodPost, cycleController.CompleteCycle, cur.Id.String(), map[string]any{"carry": true}); code != http.StatusOK {
			t.Fatalf("complete Cur, carrying: %d %s", code, out)
		}
		var in string
		if err := env.PG.QueryRow(`SELECT cycle_id::text FROM task_cycles WHERE task_uuid = $1`, task).Scan(&in); err != nil || in != cycleNamed("Third").Id.String() {
			t.Fatalf("Cur's open task went to %s (Later, complete, is %s), want Third, the first open cycle after: %v", in, cycleNamed("Later").Id, err)
		}
	})
	t.Run("an archived project's cycles don't change", func(t *testing.T) {
		if code, out := create("Spare", now.AddDate(0, 0, 60), 1); code != http.StatusOK {
			t.Fatalf("Spare: %d %s", code, out)
		}
		spare := cycleNamed("Spare").Id.String()
		if code, out := b3Call(s, "ana", http.MethodPost, cycleController.DeleteCycle, spare); code != http.StatusOK {
			t.Fatalf("delete Spare: %d %s", code, out)
		}
		s.dg.Mutate(t, []map[string]any{{"uid": s.uids["p"], "project_deleted_at": "2026-10-01T00:00:00Z"}})
		if code, out := restore(spare); code != http.StatusForbidden || !strings.Contains(out, "archived") || !deleted(spare) {
			t.Fatalf("restore a cycle in an archived project: %d %s", code, out)
		}
		code, out := s.read("ana", cycleController.ListCycles, "")
		var cycles struct {
			Data struct {
				CanEdit *bool `json:"can_edit"`
			} `json:"data"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(out), &cycles) != nil || cycles.Data.CanEdit == nil || *cycles.Data.CanEdit {
			t.Fatalf("an admin reads an archived project's cycles as read-only: %d %s", code, out)
		}
		if b3Backlog(t, s, "ana").Data.CanEdit {
			t.Fatal("an admin reads an archived project's backlog as read-only")
		}
		if code, out := setParallel("ana", true); code != http.StatusForbidden || !strings.Contains(out, "archived") {
			t.Fatalf("on in an archived project: %d %s", code, out)
		}
		if code, out := create("Late", now.AddDate(0, 0, 30), 1); code != http.StatusForbidden {
			t.Fatalf("a cycle in an archived project: %d %s", code, out)
		}
	})
}

// Migration 593 goes down and up again (and up twice) cleanly: down drops
// project_settings, up makes it with parallel cycles off by default.
func TestJ04CMigration593UpDownUp(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile("../../migrations/593_project_parallel_cycles." + name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	run := func(q string) {
		t.Helper()
		if _, err := env.PG.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exists := func() bool {
		t.Helper()
		var ok bool
		if err := env.PG.QueryRow(`SELECT to_regclass('project_settings') IS NOT NULL`).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	run(down)
	if exists() {
		t.Fatal("down left project_settings")
	}
	run(down)
	run(up)
	run(up)
	if !exists() {
		t.Fatal("up made no project_settings")
	}
	var def string
	if err := env.PG.QueryRow(`SELECT column_default FROM information_schema.columns WHERE table_name = 'project_settings' AND column_name = 'parallel_cycles' AND is_nullable = 'NO'`).Scan(&def); err != nil || def != "false" {
		t.Fatalf("parallel_cycles NOT NULL DEFAULT false: %q %v", def, err)
	}
}
