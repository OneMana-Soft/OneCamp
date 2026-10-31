//go:build integration

package integration_test

// linear-20: a project's forecast, from its pace and from what its work waits
// on, as each reader may see it. Run: integ_rel.sh <wt> <log>
// '^(TestLinear20AForecastFromPaceAndLinks|TestLinear20ACutForecastCountsEveryTask|TestLinear20AOpenCutKeepsTheSoonestDueAndSaysSo|TestLinear20ATenThousandOpenTasksAreCutAndSaidWithinTheBudget)$'

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/akashc777/OneCamp/tests/integration/dgfixture"
	"github.com/google/uuid"
)

type l20Forecast struct {
	Data struct {
		State       string  `json:"state"`
		Date        *string `json:"date"`
		Optimistic  *string `json:"optimistic"`
		Pessimistic *string `json:"pessimistic"`
		ByPace      *string `json:"by_pace"`
		ByLinks     *string `json:"by_links"`
		Open        float64 `json:"open"`
		Unit        string  `json:"unit"`
		PerWeek     float64 `json:"per_week"`
		HeldBy      []struct {
			UUID        string `json:"task_uuid"`
			Name        string `json:"task_name"`
			External    bool   `json:"external"`
			ProjectName string `json:"project_name"`
			DaysLate    int    `json:"days_late"`
		} `json:"held_by"`
		HeldCount int `json:"held_count"`
		OpenCut   *struct {
			Total   int `json:"total"`
			Counted int `json:"counted"`
		} `json:"open_cut"`
	} `json:"data"`
}

func (w *dg10World) forecast(who, project string) (int, l20Forecast, string) {
	w.t.Helper()
	code, body := w.call(who, false, taskController.ProjectForecast, http.MethodGet, "/?tz=UTC", map[string]string{"project_uuid": project}, nil)
	var f l20Forecast
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &f); err != nil {
			w.t.Fatal(err)
		}
	}
	return code, f, body
}

func TestLinear20AForecastFromPaceAndLinks(t *testing.T) {
	w := newDG10World(t)
	integration.StubMqttClient(t)
	y, m, d := time.Now().UTC().Date()
	day := func(n int) time.Time { return time.Date(y, m, d+n, 9, 0, 0, 0, time.UTC) }
	on := func(n int) string { return day(n).Format(time.DateOnly) }

	// Launch's pace: eight tasks done in the last four weeks, two a week, and
	// one done before them, which doesn't count.
	for i, ago := range []int{0, 3, 6, 10, 13, 17, 20, 27, 40} {
		w.taskAt(fmt.Sprintf("done%d", i), "p", time.Time{}, time.Time{},
			map[string]any{"task_status": "done", "task_status_since": day(-ago).Format(time.RFC3339)}, "")
	}
	// Open: b, four days late, and c, which waits on b and on Marketing's m,
	// twelve days late. Ben isn't in Marketing.
	w.taskAt("m", "q", day(-20), day(-12), nil, "")
	w.taskAt("b", "p", day(-7), day(-4), nil, "ben")
	w.taskAt("c", "p", day(-1), day(20), nil, "", "b", "m")

	// The pace says a week (two open at two a week). The links say more: b
	// and m end today at the earliest, so c, which was to start yesterday,
	// starts tomorrow and ends two days later, on the 22nd day; b and m are
	// on its critical path.
	code, ana, body := w.forecast("ana", w.p)
	if code != http.StatusOK || ana.Data.State != "forecast" || *ana.Data.ByPace != on(7) || *ana.Data.ByLinks != on(22) || *ana.Data.Date != on(22) {
		t.Fatalf("Ana's forecast: %d %s", code, body)
	}
	if ana.Data.Open != 2 || ana.Data.PerWeek != 2 || ana.Data.Unit != "tasks" || ana.Data.Optimistic == nil || ana.Data.Pessimistic == nil {
		t.Errorf("Ana's pace: %s", body)
	}
	var held []string
	for _, h := range ana.Data.HeldBy {
		held = append(held, fmt.Sprintf("%s %v %s %d", h.Name, h.External, h.ProjectName, h.DaysLate))
	}
	if want := []string{"m true Marketing 12", "b false  4"}; !reflect.DeepEqual(held, want) || ana.Data.HeldCount != 2 {
		t.Errorf("held up by Marketing's m, then b (%d in all): %v, want %v", ana.Data.HeldCount, held, want)
	}

	// Ben can't see Marketing's work: his forecast has the same date, held up
	// by b alone, and says nothing of Marketing.
	code, ben, body := w.forecast("ben", w.p)
	if code != http.StatusOK || *ben.Data.Date != on(22) || len(ben.Data.HeldBy) != 1 || ben.Data.HeldBy[0].Name != "b" ||
		strings.Contains(body, w.tasks["m"]) || strings.Contains(body, "Marketing") {
		t.Errorf("Ben's forecast: %d %s", code, body)
	}
	// Cat only reads Launch, and may still read its forecast. Ben isn't in
	// Marketing, so Marketing's forecast isn't his to read.
	if code, cat, body := w.forecast("cat", w.p); code != http.StatusOK || cat.Data.Date == nil || *cat.Data.Date != on(22) {
		t.Errorf("Cat reads Launch's forecast: %d %s", code, body)
	}
	if code, _, body := w.forecast("ben", w.q); code != http.StatusNotFound {
		t.Errorf("Ben reads Marketing's forecast: %d %s, want 404, as if it weren't there", code, body)
	}

	// With b and m done, nothing is late: the links end with c as planned,
	// on the 20th day, and nothing holds it up.
	w.dg.Mutate(t, []map[string]any{{"uid": w.uids["b"], "task_status": "done"}, {"uid": w.uids["m"], "task_status": "done"}})
	if code, ana, body := w.forecast("ana", w.p); code != http.StatusOK || *ana.Data.Date != on(20) || ana.Data.HeldBy == nil || len(ana.Data.HeldBy) != 0 {
		t.Errorf("nothing late: %d %s", code, body)
	}

	// On an estimate scale, the forecast counts as the scale does, as the
	// burndown and the velocity do: c, with no points, counts as one, and so
	// does each of the eight tasks done in the last four weeks.
	w.exec(`INSERT INTO project_estimate_scales (project_uuid, kind) VALUES ($1, 'fibonacci')`, w.p)
	if code, ana, body := w.forecast("ana", w.p); code != http.StatusOK || ana.Data.Unit != "points" || ana.Data.Open != 1 || ana.Data.PerWeek != 2 {
		t.Errorf("on a scale, in points: %d %s", code, body)
	}
}

// A very large project's forecast (main, 29 Oct 14:40): the plan read stops at
// its limit, yet the forecast counts every open task and its whole pace, each
// task of them the reader may see. With no more open tasks than the limit, the
// read keeps every one and the newest finished ones, so what a cut leaves out
// is the oldest finished work, which moves nothing: the forecast is whole and
// says nothing of a cut. The limit is three here, Launch's three open tasks,
// below its six finished ones, rather than 5,000.
func TestLinear20ACutForecastCountsEveryTask(t *testing.T) {
	w := newDG10World(t)
	integration.StubMqttClient(t)
	y, m, d := time.Now().UTC().Date()
	day := func(n int) time.Time { return time.Date(y, m, d+n, 9, 0, 0, 0, time.UTC) }
	on := func(n int) string { return day(n).Format(time.DateOnly) }
	doneOn := func(n int, more map[string]any) map[string]any {
		out := map[string]any{"task_status": "done", "task_status_since": day(n).Format(time.RFC3339)}
		for k, v := range more {
			out[k] = v
		}
		return out
	}

	// Launch's oldest work, made first, so it has the graph's lowest ids:
	// finished long ago, o0 with no day it was finished (imported, say).
	w.taskAt("o0", "p", time.Time{}, time.Time{}, map[string]any{"task_status": "done"}, "")
	for i, ago := range []int{60, 50, 40} {
		w.taskAt(fmt.Sprintf("o%d", i+1), "p", day(-ago-5), day(-ago), doneOn(-ago, nil), "")
	}
	// Open: a, dated, with 3 points; b; and r, restricted, which Ben may not see.
	w.taskAt("a", "p", day(-1), day(10), map[string]any{"task_estimate_points": 3}, "")
	w.taskAt("b", "p", time.Time{}, time.Time{}, nil, "")
	w.taskAt("r", "p", time.Time{}, time.Time{}, nil, "")
	w.exec(`INSERT INTO task_restrictions (task_id, project_id, restricted_by) VALUES ($1, $2, $3)`, w.tasks["r"], w.p, w.people["ana"])
	// The pace: two done in the last four weeks, d1, the newest, with 2 points.
	w.taskAt("d2", "p", day(-12), day(-9), doneOn(-9, nil), "")
	w.taskAt("d1", "p", day(-4), day(-2), doneOn(-2, map[string]any{"task_estimate_points": 2}), "")

	restore := taskBusiness.CapPlansForTest(3)
	defer restore()

	// The read keeps every open task and the three newest finished ones.
	asAna := taskread.ActingFor(taskread.Install(context.Background()), w.people["ana"].String())
	read, cut, err := planRead(asAna, w.p, w.people["ana"].String(), taskDomain.PlanLimits{Open: 3, Finished: 3})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "d1", "d2", "o3", "r"}; cut != (taskDomain.PlanCut{Finished: true}) || !reflect.DeepEqual(namesOf(read), want) {
		t.Errorf("a read cut at three finished tasks keeps %v (cut %+v), want %v: every open task and the newest finished ones", namesOf(read), cut, want)
	}

	// Ana sees all three open tasks, at two a week done over four weeks: six
	// weeks by the pace; the links end with a, on the 10th day.
	code, ana, body := w.forecast("ana", w.p)
	if code != http.StatusOK || ana.Data.State != "forecast" || ana.Data.Open != 3 || ana.Data.Unit != "tasks" || ana.Data.PerWeek != 0.5 {
		t.Fatalf("Ana's cut forecast counts every open task and its whole pace: %d %s", code, body)
	}
	if *ana.Data.ByPace != on(42) || *ana.Data.ByLinks != on(10) || *ana.Data.Date != on(42) {
		t.Errorf("Ana's dates: %s", body)
	}
	if strings.Contains(body, "truncated") || strings.Contains(body, "counted") {
		t.Errorf("a cut forecast is whole, and says nothing of the cut: %s", body)
	}
	// Ben may not see r: it isn't in what is open.
	code, ben, body := w.forecast("ben", w.p)
	if code != http.StatusOK || ben.Data.Open != 2 || ben.Data.PerWeek != 0.5 || *ben.Data.Date != on(28) {
		t.Errorf("Ben's cut forecast, without r: %d %s", code, body)
	}

	// On an estimate scale, in points as the scale counts them (a task with
	// none as one): 3 for a, 1 each for b and r open; 2 for d1 and 1 for d2
	// done over four weeks.
	w.exec(`INSERT INTO project_estimate_scales (project_uuid, kind) VALUES ($1, 'fibonacci')`, w.p)
	if code, ana, body := w.forecast("ana", w.p); code != http.StatusOK || ana.Data.Unit != "points" || ana.Data.Open != 5 || ana.Data.PerWeek != 0.75 {
		t.Errorf("a cut forecast on a scale, in points: %d %s", code, body)
	}

	// Uncut, the read keeps all nine, o0 too, which says no day it was finished.
	restore()
	if read, cut, err := planRead(asAna, w.p, w.people["ana"].String(), taskDomain.PlanLimits{Open: 5000, Finished: 5000}); err != nil || cut != (taskDomain.PlanCut{}) || len(read) != 9 {
		t.Errorf("an uncut read keeps all 9 of Launch's tasks: %d (cut %+v, %v)", len(read), cut, err)
	}
}

// namesOf is the tasks' names, sorted.
// planRead is project's plan as reader's plan read keeps it (one project of
// taskDomain.PlanTasksOf), and what its limits left out.
func planRead(ctx context.Context, project, reader string, limits taskDomain.PlanLimits) ([]*dgraphStruct.DgraphTask, taskDomain.PlanCut, error) {
	reads, err := taskDomain.PlanTasksOf(ctx, []string{project}, reader, limits)
	if err != nil {
		return nil, taskDomain.PlanCut{}, err
	}
	r := reads[strings.ToLower(project)]
	return r.Tasks, r.Cut, nil
}

func namesOf(tasks []*dgraphStruct.DgraphTask) []string {
	out := []string{}
	for _, k := range tasks {
		out = append(out, k.Name)
	}
	sort.Strings(out)
	return out
}

// Open tasks alone over the plan read's limit (main, 29 Oct 15:3x): the read
// keeps the project's open tasks up to the limit, of those the reader may
// see: the soonest due first, those due the same day in the graph's order,
// then those with no due day, the earliest made first and any with no day
// made last. The forecast's pace and open work count every one, and the
// answer says how many the date's links count of how many there are, in that
// case only. The limit is three here, below Launch's seven open.
func TestLinear20AOpenCutKeepsTheSoonestDueAndSaysSo(t *testing.T) {
	w := newDG10World(t)
	integration.StubMqttClient(t)
	y, m, d := time.Now().UTC().Date()
	day := func(n int) time.Time { return time.Date(y, m, d+n, 9, 0, 0, 0, time.UTC) }
	made := func(n int) map[string]any { return map[string]any{"task_created_at": day(n).Format(time.RFC3339)} }

	// Made in this order, so the graph's ids run against the days they were
	// made: e and c, with no due day, made 2 and 20 days ago; z, with neither
	// (old data, say); a and a2, both due on the 10th; b, due on the 3rd; and
	// r, due tomorrow, which Ben may not see.
	w.taskAt("e", "p", time.Time{}, time.Time{}, made(-2), "")
	w.taskAt("c", "p", time.Time{}, time.Time{}, made(-20), "")
	w.taskAt("z", "p", time.Time{}, time.Time{}, map[string]any{"task_created_at": nil}, "")
	w.taskAt("a", "p", day(-1), day(10), made(-1), "")
	w.taskAt("a2", "p", day(-1), day(10), made(-9), "")
	w.taskAt("b", "p", day(-1), day(3), made(-3), "")
	w.taskAt("r", "p", day(-1), day(1), made(-4), "")
	w.exec(`INSERT INTO task_restrictions (task_id, project_id, restricted_by) VALUES ($1, $2, $3)`, w.tasks["r"], w.p, w.people["ana"])
	// The pace: two done in the last four weeks.
	for i, ago := range []int{2, 9} {
		w.taskAt(fmt.Sprintf("d%d", i+1), "p", day(-ago-2), day(-ago), map[string]any{"task_status": "done", "task_status_since": day(-ago).Format(time.RFC3339)}, "")
	}

	ctx := taskread.Install(context.Background())
	keeps := func(who string, open int) ([]string, taskDomain.PlanCut) {
		t.Helper()
		id := w.people[who].String()
		read, cut, err := planRead(taskread.ActingFor(ctx, id), w.p, id, taskDomain.PlanLimits{Open: open, Finished: 5})
		if err != nil {
			t.Fatal(err)
		}
		return namesOf(read), cut
	}
	for _, c := range []struct {
		who  string
		open int
		want []string
		cut  bool
	}{
		{"ana", 3, []string{"a", "b", "d1", "d2", "r"}, true},                       // r, b, then a, the graph's first of the 10th
		{"ana", 5, []string{"a", "a2", "b", "c", "d1", "d2", "r"}, true},            // then c, made before e
		{"ben", 3, []string{"a", "a2", "b", "d1", "d2"}, true},                      // r, unseen, takes no place
		{"ana", 6, []string{"a", "a2", "b", "c", "d1", "d2", "e", "r"}, true},       // z, never made, last
		{"ana", 7, []string{"a", "a2", "b", "c", "d1", "d2", "e", "r", "z"}, false}, // all seven fit
		{"ana", 0, []string{"a", "a2", "b", "c", "d1", "d2", "e", "r", "z"}, false}, // no limit: a move's impact
	} {
		kept, cut := keeps(c.who, c.open)
		if !reflect.DeepEqual(kept, c.want) || cut.Open != c.cut || cut.Finished {
			t.Errorf("%s's read of %d open tasks keeps %v (cut %+v), want %v (open cut %v)", c.who, c.open, kept, cut, c.want, c.cut)
		}
	}

	restore := taskBusiness.CapPlansForTest(3)
	defer restore()
	code, ana, body := w.forecast("ana", w.p)
	if code != http.StatusOK || ana.Data.Open != 7 || ana.Data.OpenCut == nil || ana.Data.OpenCut.Total != 7 || ana.Data.OpenCut.Counted != 3 {
		t.Errorf("Ana's forecast counts all seven open, and says its links count three of them: %d %s", code, body)
	}
	code, ben, body := w.forecast("ben", w.p)
	if code != http.StatusOK || ben.Data.Open != 6 || ben.Data.OpenCut == nil || ben.Data.OpenCut.Total != 6 || ben.Data.OpenCut.Counted != 3 {
		t.Errorf("Ben's, without r: six open, three of them counted by the links: %d %s", code, body)
	}
	// The critical path says its read was cut.
	if plan, body := w.plan("ana"); !plan.Data.Truncated {
		t.Errorf("Ana's critical path, from a cut read, says so: %s", body)
	}
	// A move's impact reads every open task: it finds a2, past the cut, and
	// the project's finish before and after.
	if code, im, body := w.impact("ana", false, "a2", 20, 25); code != http.StatusOK || im.Data.Finish.Before == nil || im.Data.Finish.After == nil {
		t.Errorf("moving a2, past the cut, still reads its whole project: %d %s", code, body)
	}
	// At seven, every open task fits: nothing is said.
	restore()
	defer taskBusiness.CapPlansForTest(7)()
	if code, ana, body := w.forecast("ana", w.p); code != http.StatusOK || ana.Data.OpenCut != nil || strings.Contains(body, "open_cut") {
		t.Errorf("a forecast whose links count every open task says nothing of a cut: %d %s", code, body)
	}
}

// veryLargeForecastP95 is the forecast's budget at 10,000 open and 5,100
// finished tasks, at the limit of 1,000: most of it the reads over the whole
// project. Main set 450 ms (29 Oct 16:1x) from a temporary harness's 371-423
// ms on an earlier plan read; this test on head 1 measured 513 ms at load
// 22.4, 617 at 28.9 and 823 at 47.6 (about 238 + 12.3 x load), so the base
// was 600 ms (main, 29 Oct 17:0x). Then 703 ms at load 22, 748 at 30 and
// 1,011 at 48: 600 was too tight for this machine's noise, so the base is 800
// ms, which still catches a read that got twice as slow (main, 29 Oct 18:30).
// The forecast query head (iii) brings the measure down, with a before and
// after at the same load, and then this base to the new measure plus
// headroom. deps.forecast keeps its 180 ms on the standard seed.
const veryLargeForecastP95 = 800 * time.Millisecond

// A 15k-task read feels a busy machine that the calibration's 1 ms lookup
// doesn't: at load 47.6 it took 823 ms with the calibration at x1.00. So this
// budget alone also scales by the load it was measured under, max(1,
// load1/25), at most 3x; above load 75 such a scale says nothing, and the
// timing isn't judged (main, 29 Oct 16:5x). VERIFY's quiet baseline runs it
// unscaled, as the record.
const (
	veryLargeMeasuredLoad = 25.0
	veryLargeMaxLoadScale = 3.0
	veryLargeJudgedLoad   = 75.0
)

// veryLargeLoadScale is the budget's load factor at load, and whether a
// timing at that load is judged at all.
func veryLargeLoadScale(load float64) (scale float64, judged bool) {
	return min(max(load/veryLargeMeasuredLoad, 1), veryLargeMaxLoadScale), load <= veryLargeJudgedLoad
}

// The very-large budget's load factor: none up to the load it was measured
// at, then with the load, at most 3x; past load 75 the timing isn't judged.
func TestLinear20AVeryLargeBudgetScalesWithTheLoadUpToThreeTimes(t *testing.T) {
	for _, c := range []struct {
		load   float64
		scale  float64
		judged bool
	}{{0, 1, true}, {16, 1, true}, {25, 1, true}, {47.6, 1.904, true}, {75, 3, true}, {75.1, 3, false}, {120, 3, false}} {
		scale, judged := veryLargeLoadScale(c.load)
		if math.Abs(scale-c.scale) > 1e-9 || judged != c.judged {
			t.Errorf("at load %v: x%v, judged %v; want x%v, judged %v", c.load, scale, judged, c.scale, c.judged)
		}
	}
}

// Open tasks alone over the plan read's limit, at its real limit (main, 29 Oct
// 15:3x and 16:1x): with 10,000 open tasks, all dated and 200 of them in a
// chain, and more finished ones than the limit, the read keeps the open tasks
// due soonest up to the limit, and the latest finished ones; the forecast
// counts every open task and says how many its links count, and its p95 stays
// within the very large project's budget.
func TestLinear20ATenThousandOpenTasksAreCutAndSaidWithinTheBudget(t *testing.T) {
	w := newDG10World(t)
	integration.StubMqttClient(t)
	const open, done = 10000, 5100
	due := seedOpenWork(t, w, open, done)
	ctx := taskread.Install(context.Background())
	ana := &dgraphStruct.DgraphUser{Uid: w.uids["ana"], Uuid: w.people["ana"].String()}

	got, err := taskBusiness.Forecast(ctx, w.p, time.Now(), time.UTC, ana)
	if err != nil || got.Open != open || got.Unit != "tasks" || got.OpenCut == nil || got.OpenCut.Total != open || got.OpenCut.Counted <= 0 || got.OpenCut.Counted >= open {
		t.Fatalf("the forecast counts all %d open tasks, and says how many of them its links count: %+v %v", open, got, err)
	}
	// The read at that limit: the soonest due open tasks, none due after one it
	// leaves out, and as many of the latest finished ones.
	limit := got.OpenCut.Counted
	read, cut, err := planRead(taskread.ActingFor(ctx, ana.Uuid), w.p, ana.Uuid, taskDomain.PlanLimits{Open: limit, Finished: limit})
	if err != nil || cut != (taskDomain.PlanCut{Open: true, Finished: true}) {
		t.Fatalf("a read of %d open and %d finished at %d is cut both ways: %+v %v", open, done, limit, cut, err)
	}
	kept, finished, latest := map[string]bool{}, 0, -1
	for _, k := range read {
		if k.Status == dgraphStruct.TASK_STATUS_DONE {
			finished++
			continue
		}
		kept[k.Name] = true
		latest = max(latest, due[k.Name])
	}
	if len(kept) != limit || finished != limit {
		t.Fatalf("the read keeps %d open and %d finished, want %d of each", len(kept), finished, limit)
	}
	for name, d := range due {
		if !kept[name] && d < latest {
			t.Fatalf("%s, due on day %d, is left out while the read keeps one due on day %d", name, d, latest)
		}
	}

	run := func(ctx context.Context, _ *dgfixture.Fixture) (int, error) {
		got, err := taskBusiness.Forecast(ctx, w.p, time.Now(), time.UTC, ana)
		if err != nil {
			return 0, err
		}
		return int(got.Open), nil
	}
	// As the budget table does (TestDgraphQueryBudgets), a read over its budget
	// is measured once more before it fails: a burst on a busy machine passes,
	// a read that got slower doesn't.
	var p95, allowed time.Duration
	var load float64
	var judged bool
	for attempt := 1; ; attempt++ {
		scale, cal := calibrate(ctx, t, ana.Uuid)
		var err error
		_, p95, _, _, err = measure(ctx, nil, run, &roundTrips{})
		if err != nil {
			t.Fatalf("the forecast of %d open tasks cut at %d: %v", open, limit, err)
		}
		load, _ = strconv.ParseFloat(load1(), 64)
		var byLoad float64
		byLoad, judged = veryLargeLoadScale(load)
		allowed = time.Duration(float64(veryLargeForecastP95) * scale * byLoad)
		t.Logf("the forecast at %d open (links count %d) and %d finished, measure %d: p95 %v, budget %v (x%.2f by the calibration %v, x%.2f by the load %.2f)", open, limit, done, attempt, p95, allowed, scale, cal, byLoad, load)
		if !judged || p95 <= allowed || attempt == 2 {
			break
		}
	}
	switch {
	case !judged:
		t.Logf("the forecast's timing isn't judged: at load %.2f (over %.0f) even a %.0fx budget says nothing", load, veryLargeJudgedLoad, veryLargeMaxLoadScale)
	case p95 > allowed:
		t.Errorf("the forecast of %d open tasks cut at %d: p95 %v over its budget %v, twice", open, limit, p95, allowed)
	}
}

// seedOpenWork adds open tasks to Launch, each dated (task i due on day i%60
// + 3, all made on the same day) and the first 200 in a chain, then done
// finished ones, straight to the graph in batches; it returns each open task's
// due day.
func seedOpenWork(t *testing.T, w *dg10World, open, done int) map[string]int {
	t.Helper()
	y, m, d := time.Now().UTC().Date()
	day := func(n int) string { return time.Date(y, m, d+n, 9, 0, 0, 0, time.UTC).Format(time.RFC3339) }
	const batch = 1000
	due := map[string]int{}
	for from := 0; from < open+done; from += batch {
		var nodes, tasks []map[string]any
		for i := from; i < min(from+batch, open+done); i++ {
			key := fmt.Sprintf("t%d", i)
			n := map[string]any{"uid": "_:" + key, "dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_name": key,
				"task_status": "todo", "task_deleted_at": "0001-01-01T00:00:00Z", "task_created_at": day(-30),
				"task_project": map[string]any{"uid": w.uids["p"]}, "task_created_by": map[string]any{"uid": w.uids["ana"]},
				"task_start_date": day(i % 60), "task_due_date": day(i%60 + 3)}
			switch {
			case i >= open:
				n["task_status"], n["task_status_since"] = dgraphStruct.TASK_STATUS_DONE, day(-(i % 27))
			case i > 0 && i < 200:
				n["task_blocked_by"] = []map[string]any{{"uid": fmt.Sprintf("_:t%d", i-1)}}
			}
			if i < open {
				due[key] = i%60 + 3
			}
			nodes = append(nodes, n)
			tasks = append(tasks, map[string]any{"uid": "_:" + key})
		}
		w.dg.Mutate(t, append(nodes, map[string]any{"uid": w.uids["p"], "project_tasks": tasks}))
	}
	return due
}
