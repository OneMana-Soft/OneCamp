//go:build integration

package integration_test

// linear-13: a capacity a person sets for each cycle. The backlog plans that
// cycle against it, and every other against the project's own (what its
// recent cycles finished). Only the project's admins set one.
// Run: integ_rel.sh <wt> <log> 'TestL13'

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestL13AnAdminSetsACyclesCapacity(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.addTasks(2)
	project := mustUUID(t, s.project)
	now := time.Now().UTC()
	cur, err := cycleModel.Create(project, "", now.AddDate(0, 0, -3), now.AddDate(0, 0, 11), s.people["ana"])
	if err != nil {
		t.Fatal(err)
	}
	next, err := cycleModel.Create(project, "", now.AddDate(0, 0, 11), now.AddDate(0, 0, 25), s.people["ana"])
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
	set := func(who string, cycle uuid.UUID, minutes any) (int, string) {
		t.Helper()
		return b3Do(s, who, http.MethodPost, cycleController.SetCycleCapacity, cycle.String(), map[string]any{"capacity_minutes": minutes})
	}
	planned := func(id uuid.UUID) *int {
		t.Helper()
		for _, c := range b3Backlog(t, s, "ana").Data.Backlog.Cycles {
			if c.ID == id.String() {
				return c.Capacity
			}
		}
		t.Fatalf("cycle %s isn't in the backlog", id)
		return nil
	}
	stored := func(id uuid.UUID) *int {
		t.Helper()
		c, err := cycleModel.Get(id)
		if err != nil || c == nil {
			t.Fatalf("read cycle %s: %+v %v", id, c, err)
		}
		return c.CapacityMinutes
	}

	t.Run("an admin sets one; the backlog plans that cycle with it and the others with the project's", func(t *testing.T) {
		if code, out := set("ana", next.Id, 2400); code != http.StatusOK {
			t.Fatalf("Ana sets 40 hours: %d %s", code, out)
		}
		if got := planned(next.Id); got == nil || *got != 2400 {
			t.Errorf("the next cycle plans with %v, not 2400", got)
		}
		if got := planned(cur.Id); got != nil {
			t.Errorf("the current cycle has a capacity of its own: %d", *got)
		}
	})

	t.Run("a member, someone outside, and an admin naming another project's cycle change nothing", func(t *testing.T) {
		s.person("cy", false)
		for _, c := range []struct {
			what  string
			who   string
			cycle uuid.UUID
			want  int
		}{
			{"Ben, a member", "ben", next.Id, http.StatusForbidden},
			{"Cy, outside the project", "cy", next.Id, http.StatusNotFound},
			{"Ana, of another project's cycle", "ana", elsewhere.Id, http.StatusNotFound},
		} {
			if code, out := set(c.who, c.cycle, 60); code != c.want {
				t.Errorf("%s sets a capacity: %d %s", c.what, code, out)
			}
		}
		if got := stored(next.Id); got == nil || *got != 2400 {
			t.Errorf("the next cycle's capacity changed: %v", got)
		}
		if got := stored(elsewhere.Id); got != nil {
			t.Errorf("another project's cycle has a capacity: %d", *got)
		}
	})

	t.Run("nonsense and a complete cycle are refused in words", func(t *testing.T) {
		for _, c := range []struct {
			what    string
			cycle   uuid.UUID
			minutes any
			msg     string
		}{
			{"none", next.Id, 0, "A cycle's capacity is between 1 minute and 100,000 hours."},
			{"less than none", next.Id, -60, "A cycle's capacity is between 1 minute and 100,000 hours."},
			{"more than 100,000 hours", next.Id, 6000001, "A cycle's capacity is between 1 minute and 100,000 hours."},
			{"a complete cycle's", done.Id, 60, "That cycle is complete. Its capacity stays as it ran."},
		} {
			if code, out := set("ana", c.cycle, c.minutes); code != http.StatusBadRequest || !strings.Contains(out, c.msg) {
				t.Errorf("%s: %d %s", c.what, code, out)
			}
		}
		if got := stored(next.Id); got == nil || *got != 2400 {
			t.Errorf("a refused capacity changed it: %v", got)
		}
	})

	t.Run("cleared, the project's own applies again", func(t *testing.T) {
		if code, out := set("ana", next.Id, nil); code != http.StatusOK {
			t.Fatalf("Ana clears it: %d %s", code, out)
		}
		if got := planned(next.Id); got != nil {
			t.Errorf("the next cycle still plans with %d", *got)
		}
	})
}
