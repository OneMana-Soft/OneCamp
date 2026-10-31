//go:build integration

package integration_test

// linear-05: a project estimates in hours, or in points on one of Linear's
// four scales. Its admins set the scale, whoever may edit a task gives it
// points (a size on the scale), and the backlog, burndown and capacity count
// them, a task with none as the scale says. Hours stay beside points.
// Run: integ_rel.sh <wt> <log> 'TestLinear05'

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	estimateScaleController "github.com/akashc777/OneCamp/controllers/EstimateScale"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/akashc777/OneCamp/tests/integration"
)

type l05Scale struct {
	Data struct {
		Scale *struct {
			Kind             string    `json:"kind"`
			Extended         bool      `json:"extended"`
			AllowZero        bool      `json:"allow_zero"`
			CountUnestimated bool      `json:"count_unestimated"`
			Sizes            []float64 `json:"sizes"`
			Labels           []string  `json:"labels"`
		} `json:"scale"`
		Kinds   []struct{ Kind string } `json:"kinds"`
		CanEdit bool                    `json:"can_edit"`
	} `json:"data"`
	Msg string `json:"msg"`
}

func l05ReadScale(s *wa2Scale, who string) (int, l05Scale) {
	s.t.Helper()
	code, out := b3Call(s, who, http.MethodGet, estimateScaleController.GetScale, "")
	var got l05Scale
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			s.t.Fatalf("the scale as %s: %v in %s", who, err, out)
		}
	}
	return code, got
}

func l05SetScale(s *wa2Scale, who string, scale map[string]any) (int, string) {
	s.t.Helper()
	return b3Do(s, who, http.MethodPost, estimateScaleController.SetScale, "", scale)
}

func TestLinear05AProjectsScaleIsItsAdminsToSet(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.person("vic", true)
	s.person("cy", false)

	t.Run("a project estimates in hours until an admin picks a scale", func(t *testing.T) {
		code, got := l05ReadScale(s, "ben")
		if code != http.StatusOK || got.Data.Scale != nil || len(got.Data.Kinds) != 4 || got.Data.CanEdit {
			t.Fatalf("Ben, a member, reads %d %+v", code, got.Data)
		}
		if _, got := l05ReadScale(s, "ana"); !got.Data.CanEdit {
			t.Fatal("Ana, its admin, may change it")
		}
	})

	t.Run("a member, a viewer and someone outside set nothing, and someone outside reads nothing", func(t *testing.T) {
		for who, want := range map[string]int{"ben": http.StatusForbidden, "vic": http.StatusForbidden, "cy": http.StatusNotFound} {
			if code, out := l05SetScale(s, who, map[string]any{"kind": "fibonacci"}); code != want {
				t.Errorf("%s sets a scale: %d %s, want %d", who, code, out, want)
			}
		}
		if code, _ := l05ReadScale(s, "cy"); code != http.StatusNotFound {
			t.Errorf("Cy, outside, reads the scale: %d", code)
		}
		if _, got := l05ReadScale(s, "ana"); got.Data.Scale != nil {
			t.Fatalf("the project changed scale: %+v", *got.Data.Scale)
		}
	})

	t.Run("an admin sets the T-shirt scale, and everyone in the project reads its sizes", func(t *testing.T) {
		if code, out := l05SetScale(s, "ana", map[string]any{"kind": "tshirt", "extended": true, "count_unestimated": true}); code != http.StatusOK {
			t.Fatalf("Ana sets T-shirt sizes: %d %s", code, out)
		}
		code, got := l05ReadScale(s, "vic")
		sc := got.Data.Scale
		if code != http.StatusOK || sc == nil || sc.Kind != "tshirt" || !sc.Extended || sc.AllowZero || !sc.CountUnestimated {
			t.Fatalf("Vic, a viewer, reads %d %+v", code, sc)
		}
		if !slices.Equal(sc.Sizes, []float64{1, 2, 3, 5, 8, 13, 21}) || !slices.Equal(sc.Labels, []string{"XS", "S", "M", "L", "XL", "XXL", "XXXL"}) {
			t.Fatalf("sizes %v labels %v", sc.Sizes, sc.Labels)
		}
	})

	t.Run("a scale that isn't one is refused in words, and hours puts the project back", func(t *testing.T) {
		code, out := l05SetScale(s, "ana", map[string]any{"kind": "days"})
		if code != http.StatusBadRequest || !strings.Contains(out, "exponential, Fibonacci, linear or T-shirt") {
			t.Fatalf("a scale of days: %d %s", code, out)
		}
		if _, got := l05ReadScale(s, "ana"); got.Data.Scale == nil || got.Data.Scale.Kind != "tshirt" {
			t.Fatal("a refused scale changed the project's")
		}
		if code, out := l05SetScale(s, "ana", map[string]any{"kind": "hours"}); code != http.StatusOK {
			t.Fatalf("back to hours: %d %s", code, out)
		}
		if _, got := l05ReadScale(s, "ben"); got.Data.Scale != nil {
			t.Fatalf("the project still counts in points: %+v", *got.Data.Scale)
		}
	})
}

// l05Points is a task's points in the graph, nil for none.
// l05UID is the graph's uid of the one node whose pred is value.
func l05UID(s *wa2Scale, pred, value string) string {
	s.t.Helper()
	var out struct {
		N []struct {
			UID string `json:"uid"`
		} `json:"n"`
	}
	if err := json.Unmarshal(s.dg.Query(s.t, fmt.Sprintf(`{ n(func: eq(%s, %q)) { uid } }`, pred, value)), &out); err != nil || len(out.N) != 1 {
		s.t.Fatalf("the graph's node with %s %s: %v %+v", pred, value, err, out)
	}
	return out.N[0].UID
}

func l05Points(s *wa2Scale, task string) *float64 {
	s.t.Helper()
	var out struct {
		T []struct {
			Points *float64 `json:"task_estimate_points"`
		} `json:"t"`
	}
	if err := json.Unmarshal(s.dg.Query(s.t, fmt.Sprintf(`{ t(func: eq(task_uuid, %q)) { task_uuid task_estimate_points } }`, task)), &out); err != nil || len(out.T) != 1 {
		s.t.Fatalf("read task %s: %v %+v", task, err, out)
	}
	return out.T[0].Points
}

func TestLinear05PointsAreASizeOnTheProjectsScale(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.addTasks(1)
	task := s.tasks[0]
	s.person("vic", true)
	s.person("cy", false)
	give := func(who string, points any) (int, string) {
		t.Helper()
		return b3Do(s, who, http.MethodPost, taskController.UpdateTaskPoints, "", map[string]any{"task_uuid": task, "task_estimate_points": points})
	}
	is := func(want *float64) {
		t.Helper()
		got := l05Points(s, task)
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Fatalf("the task's points are %v, want %v", got, want)
		}
	}
	pts := func(v float64) *float64 { return &v }

	t.Run("a project in hours takes no points", func(t *testing.T) {
		if code, out := give("ana", 3); code != http.StatusBadRequest || !strings.Contains(out, "This task's project estimates in hours.") {
			t.Fatalf("points in a project in hours: %d %s", code, out)
		}
		is(nil)
	})

	if code, out := l05SetScale(s, "ana", map[string]any{"kind": "fibonacci", "count_unestimated": true}); code != http.StatusOK {
		t.Fatalf("Ana sets Fibonacci: %d %s", code, out)
	}

	t.Run("whoever may edit the task gives it a size, and nothing else", func(t *testing.T) {
		if code, out := give("ana", 5); code != http.StatusOK {
			t.Fatalf("Ana gives it 5: %d %s", code, out)
		}
		is(pts(5))
		if code, out := give("ben", 8); code != http.StatusOK {
			t.Fatalf("Ben, a member, gives it 8: %d %s", code, out)
		}
		is(pts(8))
		for _, off := range []any{4, 0, 2.5, 13} {
			if code, out := give("ana", off); code != http.StatusBadRequest || !strings.Contains(out, "aren't a size on this project's scale") {
				t.Errorf("%v isn't a size on Fibonacci: %d %s", off, code, out)
			}
		}
		is(pts(8))
	})

	t.Run("a viewer and someone outside change nothing", func(t *testing.T) {
		for who, want := range map[string]int{"vic": http.StatusForbidden, "cy": http.StatusNotFound} {
			if code, out := give(who, 3); code != want {
				t.Errorf("%s gives it points: %d %s, want %d", who, code, out, want)
			}
		}
		is(pts(8))
	})

	t.Run("the task's own answer carries the scale its Points row sizes on", func(t *testing.T) {
		// The panel asks for the project's parts with the task (?include=project_meta):
		// the scale comes in the same answer, so the row paints with the task, for
		// whoever may see it; only an admin of the project may change the scale.
		for who, admin := range map[string]bool{"ana": true, "ben": false, "vic": false} {
			code, out := s.readWith(who, taskController.GetTaskInfo, map[string]string{"task_uuid": task}, "?include=project_meta")
			if code != http.StatusOK {
				t.Fatalf("%s reads the task: %d %s", who, code, out)
			}
			var got struct {
				Included struct {
					Estimates struct {
						Data struct {
							Scale *struct {
								Kind  string    `json:"kind"`
								Sizes []float64 `json:"sizes"`
							} `json:"scale"`
							CanEdit bool `json:"can_edit"`
						} `json:"data"`
					} `json:"estimates"`
				} `json:"included"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			if e := got.Included.Estimates.Data; e.Scale == nil || e.Scale.Kind != "fibonacci" || len(e.Scale.Sizes) != 5 || e.CanEdit != admin {
				t.Errorf("%s: the task's answer carries %s", who, out)
			}
		}
		if code, out := s.readWith("cy", taskController.GetTaskInfo, map[string]string{"task_uuid": task}, "?include=project_meta"); code != http.StatusNotFound {
			t.Errorf("cy, outside the project, reads the task: %d %s", code, out)
		}
		// Someone outside the project who is assigned one of its tasks (oz) may open that
		// task, not its project: the answer is the task's, with no estimates part, so the
		// panel draws no Points row for oz. (A lower-role pin: the include already left
		// the part out before the panel's fix.)
		s.person("oz", false)
		s.addTasks(1)
		ozTask := s.tasks[len(s.tasks)-1]
		taskUID, ozUID := l05UID(s, "task_uuid", ozTask), l05UID(s, "user_uuid", s.people["oz"].String())
		s.dg.Mutate(t, []map[string]any{{"uid": taskUID, "task_assignee": map[string]any{"uid": ozUID}}, {"uid": ozUID, "user_tasks": []map[string]any{{"uid": taskUID}}}})
		code, out := s.readWith("oz", taskController.GetTaskInfo, map[string]string{"task_uuid": ozTask}, "?include=project_meta")
		if code != http.StatusOK {
			t.Fatalf("oz, the task's assignee from outside its project, reads it: %d %s", code, out)
		}
		var answer struct {
			Included map[string]json.RawMessage `json:"included"`
		}
		if err := json.Unmarshal([]byte(out), &answer); err != nil {
			t.Fatal(err)
		}
		if part, has := answer.Included["estimates"]; has {
			t.Errorf("oz, outside the project, is given its estimates: %s", part)
		}
	})

	t.Run("the task's history says each change", func(t *testing.T) {
		var out struct {
			T []struct {
				Changes []struct {
					Prev string `json:"activity_prev_state"`
					Next string `json:"activity_next_state"`
				} `json:"task_activities"`
			} `json:"t"`
		}
		q := fmt.Sprintf(`{ t(func: eq(task_uuid, %q)) { task_activities(orderasc: activity_time) @filter(eq(activity_type, "points")) { activity_prev_state activity_next_state } } }`, task)
		if err := json.Unmarshal(s.dg.Query(t, q), &out); err != nil || len(out.T) != 1 {
			t.Fatalf("history: %v %+v", err, out)
		}
		var got []string
		for _, c := range out.T[0].Changes {
			got = append(got, c.Prev+">"+c.Next)
		}
		if !slices.Equal(got, []string{">5", "5>8"}) {
			t.Fatalf("history %v, want [>5 5>8]", got)
		}
	})

	t.Run("null takes the points off, and zero is a size once the scale allows it", func(t *testing.T) {
		if code, out := give("ana", nil); code != http.StatusOK {
			t.Fatalf("Ana takes the points off: %d %s", code, out)
		}
		is(nil)
		if code, out := l05SetScale(s, "ana", map[string]any{"kind": "fibonacci", "allow_zero": true}); code != http.StatusOK {
			t.Fatalf("Ana allows zero: %d %s", code, out)
		}
		if code, out := give("ana", 0); code != http.StatusOK {
			t.Fatalf("Ana gives it 0: %d %s", code, out)
		}
		is(pts(0))
	})
}

type l05Backlog struct {
	Data struct {
		Backlog struct {
			Cycles []struct {
				ID             string   `json:"id"`
				EstimatePoints *float64 `json:"estimate_points"`
				CapacityPoints *int     `json:"capacity_points"`
				CapacityHours  *int     `json:"capacity_minutes"`
			} `json:"cycles"`
			EstimatePoints *float64 `json:"estimate_points"`
			Scale          *struct {
				Kind  string    `json:"kind"`
				Sizes []float64 `json:"sizes"`
			} `json:"scale"`
		} `json:"backlog"`
	} `json:"data"`
}

type l05Burndown struct {
	Data struct {
		Burndown struct {
			ScopePoints     []float64 `json:"scope_points"`
			RemainingPoints []float64 `json:"remaining_points"`
			Points          float64   `json:"points"`
			Pointed         int       `json:"pointed"`
		} `json:"burndown"`
	} `json:"data"`
}

func TestLinear05TheBacklogBurndownAndCapacityCountPoints(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.addTasks(3) // to do, in progress, in review
	project := mustUUID(t, s.project)
	now := time.Now().UTC()
	cur, err := cycleModel.Create(project, "", now.AddDate(0, 0, -3), now.AddDate(0, 0, 11), s.people["ana"])
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range s.tasks {
		if err := cycleModel.SetTask(mustUUID(t, id), &cur.Id); err != nil {
			t.Fatal(err)
		}
	}
	backlog := func() l05Backlog {
		t.Helper()
		code, out := b3Call(s, "ben", http.MethodGet, cycleController.Backlog, "")
		var got l05Backlog
		if code != http.StatusOK || json.Unmarshal([]byte(out), &got) != nil || len(got.Data.Backlog.Cycles) != 1 {
			t.Fatalf("the backlog as Ben: %d %s", code, out)
		}
		return got
	}
	if b := backlog().Data.Backlog; b.Scale != nil {
		t.Fatalf("a project in hours: its backlog carries a scale: %+v", b.Scale)
	}
	if code, out := l05SetScale(s, "ana", map[string]any{"kind": "fibonacci", "count_unestimated": true}); code != http.StatusOK {
		t.Fatalf("Ana sets Fibonacci: %d %s", code, out)
	}
	for i, p := range []float64{3, 5} {
		if code, out := b3Do(s, "ana", http.MethodPost, taskController.UpdateTaskPoints, "", map[string]any{"task_uuid": s.tasks[i], "task_estimate_points": p}); code != http.StatusOK {
			t.Fatalf("task %d gets %v: %d %s", i, p, code, out)
		}
	}

	burndown := func() l05Burndown {
		t.Helper()
		code, out := b3Call(s, "ben", http.MethodGet, cycleController.CycleBurndown, cur.Id.String())
		var got l05Burndown
		if code != http.StatusOK || json.Unmarshal([]byte(out), &got) != nil {
			t.Fatalf("the burndown as Ben: %d %s", code, out)
		}
		return got
	}
	capacity := func(who string, body map[string]any) (int, string) {
		t.Helper()
		return b3Do(s, who, http.MethodPost, cycleController.SetCycleCapacity, cur.Id.String(), body)
	}

	t.Run("the cycle and its burndown count 3 and 5, and one for the task with none", func(t *testing.T) {
		b := backlog().Data.Backlog
		if b.Scale == nil || b.Scale.Kind != "fibonacci" || len(b.Scale.Sizes) == 0 {
			t.Errorf("the backlog carries the scale it counts in: %+v", b.Scale)
		}
		if p := b.Cycles[0].EstimatePoints; p == nil || *p != 9 {
			t.Errorf("the cycle's points: %v, want 9", p)
		}
		if b.EstimatePoints == nil || *b.EstimatePoints != 0 {
			t.Errorf("the backlog (everything is in the cycle) has %v points, want 0", b.EstimatePoints)
		}
		d := burndown().Data.Burndown
		if d.Points != 9 || d.Pointed != 2 || len(d.ScopePoints) == 0 || d.ScopePoints[len(d.ScopePoints)-1] != 9 || d.RemainingPoints[len(d.RemainingPoints)-1] != 9 {
			t.Errorf("burndown: %+v", d)
		}
	})

	t.Run("an admin sets the cycle's capacity in points, not hours; a member sets nothing", func(t *testing.T) {
		if code, out := capacity("ana", map[string]any{"capacity_points": 20}); code != http.StatusOK {
			t.Fatalf("Ana plans 20 points: %d %s", code, out)
		}
		if c := backlog().Data.Backlog.Cycles[0]; c.CapacityPoints == nil || *c.CapacityPoints != 20 {
			t.Fatalf("the cycle plans with %v points", c.CapacityPoints)
		}
		for _, c := range []struct {
			who  string
			body map[string]any
			code int
			msg  string
		}{
			{"ana", map[string]any{"capacity_minutes": 600}, http.StatusBadRequest, "This project estimates in points, so its capacity is in points too."},
			{"ana", map[string]any{"capacity_points": 0}, http.StatusBadRequest, "between 1 and 100,000 points"},
			{"ana", map[string]any{"capacity_points": 100001}, http.StatusBadRequest, "between 1 and 100,000 points"},
			{"ben", map[string]any{"capacity_points": 5}, http.StatusForbidden, ""},
		} {
			if code, out := capacity(c.who, c.body); code != c.code || !strings.Contains(out, c.msg) {
				t.Errorf("%s sends %v: %d %s", c.who, c.body, code, out)
			}
		}
		if c := backlog().Data.Backlog.Cycles[0]; *c.CapacityPoints != 20 || c.CapacityHours != nil {
			t.Fatalf("a refused capacity changed the cycle's: %v points, %v minutes", *c.CapacityPoints, c.CapacityHours)
		}
		if code, out := capacity("ana", map[string]any{"capacity_points": nil}); code != http.StatusOK || backlog().Data.Backlog.Cycles[0].CapacityPoints != nil {
			t.Fatalf("null clears it: %d %s", code, out)
		}
	})

	t.Run("back on hours, nothing counts points and capacity is in hours", func(t *testing.T) {
		if code, out := l05SetScale(s, "ana", map[string]any{"kind": "hours"}); code != http.StatusOK {
			t.Fatalf("back to hours: %d %s", code, out)
		}
		if b := backlog().Data.Backlog; b.EstimatePoints != nil || b.Cycles[0].EstimatePoints != nil {
			t.Errorf("points in a project in hours: %v %v", b.EstimatePoints, b.Cycles[0].EstimatePoints)
		}
		if d := burndown().Data.Burndown; d.ScopePoints != nil || d.Points != 0 {
			t.Errorf("a burndown in points in a project in hours: %+v", d)
		}
		if code, out := capacity("ana", map[string]any{"capacity_points": 5}); code != http.StatusBadRequest || !strings.Contains(out, "This project estimates in hours, so its capacity is in hours too.") {
			t.Errorf("points as capacity in a project in hours: %d %s", code, out)
		}
		// The tasks keep their points, unread until the project counts in them again.
		if p := l05Points(s, s.tasks[0]); p == nil || *p != 3 {
			t.Errorf("the task's points went with the scale: %v", p)
		}
	})
}
