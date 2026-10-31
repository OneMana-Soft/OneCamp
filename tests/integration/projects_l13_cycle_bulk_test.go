//go:build integration

package integration_test

// linear-13: planning a cycle in bulk. Tasks go into one of their project's
// open cycles, or back to the backlog, many in one request (POST /task/bulk
// op "cycle", as X picks them in the backlog), each checked as one would be.
// Run: integ_rel.sh <wt> <log> 'TestL13'

import (
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestL13ManyTasksGoIntoACycleInOneRequest(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.addTasks(6)
	project := mustUUID(t, s.project)
	now := time.Now().UTC()
	cur, err := cycleModel.Create(project, "", now.AddDate(0, 0, -3), now.AddDate(0, 0, 11), s.people["ana"])
	if err != nil {
		t.Fatal(err)
	}
	done, err := cycleModel.Create(project, "", now.AddDate(0, 0, -30), now.AddDate(0, 0, -16), s.people["ana"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cycleModel.Complete(done.Id, 0, nil, nil); err != nil {
		t.Fatal(err)
	}
	elsewhere, err := cycleModel.Create(uuid.New(), "", now, now.AddDate(0, 0, 14), s.people["ana"])
	if err != nil {
		t.Fatal(err)
	}
	cycleOf := func(id string) string {
		t.Helper()
		c, err := cycleModel.CycleOf(mustUUID(t, id))
		if err != nil {
			t.Fatal(err)
		}
		if c == nil {
			return ""
		}
		return c.Id.String()
	}
	sorted := func(ids []string) string {
		out := append([]string(nil), ids...)
		for i := range out {
			out[i] = strings.ToLower(out[i])
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	three := s.tasks[:3]

	t.Run("an admin puts three in the current cycle at once; the one already there keeps when it joined", func(t *testing.T) {
		if err := cycleModel.SetTask(mustUUID(t, three[0]), &cur.Id); err != nil {
			t.Fatal(err)
		}
		// It joined yesterday; a rewrite would make that now.
		s.exec(`UPDATE task_cycles SET added_at = NOW() - interval '1 day' WHERE task_uuid = $1`, three[0])
		joined := addedAt(t, cur.Id, three[0])
		code, a := pr2Bulk(t, s, "ana", three, "cycle", cur.Id.String())
		if code != http.StatusOK || sorted(a.Data.Changed) != sorted(three[1:]) || sorted(a.Data.Unchanged) != sorted(three[:1]) || len(a.Data.Refused) != 0 {
			t.Fatalf("Ana's bulk: %d %+v", code, a)
		}
		for _, id := range three {
			if got := cycleOf(id); got != cur.Id.String() {
				t.Errorf("task %s is in %q, not the current cycle", id, got)
			}
		}
		if again := addedAt(t, cur.Id, three[0]); !again.Equal(joined) {
			t.Errorf("the task already in the cycle rejoined it: %v, then %v", joined, again)
		}
	})

	t.Run("they go back to the backlog the same way", func(t *testing.T) {
		code, a := pr2Bulk(t, s, "ana", three, "cycle", "")
		if code != http.StatusOK || sorted(a.Data.Changed) != sorted(three) {
			t.Fatalf("to the backlog: %d %+v", code, a)
		}
		for _, id := range three {
			if got := cycleOf(id); got != "" {
				t.Errorf("task %s is still in %s", id, got)
			}
		}
	})

	t.Run("a member who may change the tasks plans them too", func(t *testing.T) {
		code, a := pr2Bulk(t, s, "ben", three[:1], "cycle", cur.Id.String())
		if code != http.StatusOK || len(a.Data.Changed) != 1 || cycleOf(three[0]) != cur.Id.String() {
			t.Fatalf("Ben's bulk: %d %+v", code, a)
		}
	})

	t.Run("someone outside the project moves none of them", func(t *testing.T) {
		s.person("cy", false)
		code, a := pr2Bulk(t, s, "cy", three, "cycle", "")
		if code != http.StatusOK || len(a.Data.Changed) != 0 || len(a.Data.Refused) != 3 {
			t.Fatalf("Cy's bulk: %d %+v", code, a)
		}
		if cycleOf(three[0]) != cur.Id.String() {
			t.Errorf("Cy took a task out of its cycle")
		}
	})

	t.Run("another project's cycle refuses each task; a complete or made-up one, the request", func(t *testing.T) {
		code, a := pr2Bulk(t, s, "ana", s.tasks[3:4], "cycle", elsewhere.Id.String())
		if code != http.StatusOK || len(a.Data.Refused) != 1 || a.Data.Refused[0].Msg != "That isn't one of this project's cycles." || cycleOf(s.tasks[3]) != "" {
			t.Errorf("into another project's cycle: %d %+v", code, a)
		}
		for _, c := range []struct{ what, cycle, msg string }{
			{"a complete cycle", done.Id.String(), "That cycle is complete. Pick a current or upcoming one."},
			{"no cycle there", uuid.NewString(), "That isn't one of this project's cycles."},
			{"not an id", "next week", "That isn't one of this project's cycles."},
		} {
			if code, a := pr2Bulk(t, s, "ana", s.tasks[3:4], "cycle", c.cycle); code != http.StatusBadRequest || a.Msg != c.msg {
				t.Errorf("%s: %d %+v", c.what, code, a)
			}
		}
		if cycleOf(s.tasks[3]) != "" {
			t.Errorf("a refused request moved the task")
		}
	})
}

// addedAt is when task joined cycle.
func addedAt(t *testing.T, cycle uuid.UUID, task string) time.Time {
	t.Helper()
	members, err := cycleModel.MembersOf(cycle)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if strings.EqualFold(m.TaskUUID, task) {
			return m.AddedAt
		}
	}
	t.Fatalf("task %s isn't in cycle %s", task, cycle)
	return time.Time{}
}
