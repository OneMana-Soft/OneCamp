package business

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	estimateScale "github.com/akashc777/OneCamp/business/EstimateScale"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers/topo"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// The forecast's day: 29 Oct 2026, noon in UTC.
var forecastNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func oct(d int) *time.Time   { return at(time.UTC, 2026, 10, d, 9) }
func nov(d int) *time.Time   { return at(time.UTC, 2026, 11, d, 9) }
func pts(p float64) *float64 { return &p }

// inProject puts t in project ("p" is Launch, the project forecast; "q" is
// Marketing) and returns it.
func inProject(t *dgraphStruct.DgraphTask, project string) *dgraphStruct.DgraphTask {
	t.Project = &dgraphStruct.DgraphProject{Uuid: project, Name: map[string]string{"p": "Launch", "q": "Marketing"}[project]}
	return t
}

// done is a task of p done on since; open an undated open one of p.
func done(n int, since *time.Time) *dgraphStruct.DgraphTask {
	t := inProject(waits(fmt.Sprintf("d0000000-0000-4000-8000-%012d", n), nil, nil), "p")
	t.Status, t.StatusSince = dgraphStruct.TASK_STATUS_DONE, since
	return t
}

func open(n int) *dgraphStruct.DgraphTask {
	return inProject(waits(fmt.Sprintf("00000000-0000-4000-8000-%012d", n), nil, nil), "p")
}

// totalsOf is what the graph counts of project's tasks among tasks
// (domain.ReadPlanTotalsOf over bounds): every one, the open ones, those done in
// each span of bounds, the first done, and the day the project was made.
func totalsOf(project string, tasks []*dgraphStruct.DgraphTask, bounds []time.Time) domain.PlanTotals {
	out := domain.PlanTotals{Done: make([]domain.Tally, max(0, len(bounds)-1))}
	tally := func(c *domain.Tally, t *dgraphStruct.DgraphTask) {
		c.N++
		if t.EstimatePoints != nil {
			c.Pointed++
			c.Points += *t.EstimatePoints
		}
	}
	for _, t := range tasks {
		if t == nil || t.Project == nil || !strings.EqualFold(t.Project.Uuid, project) {
			continue
		}
		out.Tasks++
		if out.CreatedAt == nil {
			out.CreatedAt = t.Project.CreatedAt
		}
		switch {
		case !closed(t):
			tally(&out.Open, t)
		case t.Status == dgraphStruct.TASK_STATUS_DONE && t.StatusSince != nil:
			if out.FirstDone == nil || t.StatusSince.Before(*out.FirstDone) {
				out.FirstDone = t.StatusSince
			}
			for i := range out.Done {
				if !t.StatusSince.Before(bounds[i+1]) && t.StatusSince.Before(bounds[i]) {
					tally(&out.Done[i], t)
				}
			}
		}
	}
	return out
}

// buildFrom is BuildForecast with the totals the graph would count of tasks.
func buildFrom(project string, tasks []*dgraphStruct.DgraphTask, scale *estimateScale.Scale, now time.Time, loc *time.Location) ForecastAnswer {
	return BuildForecast(project, tasks, totalsOf(project, tasks, paceBounds(topo.CivilDay(now, loc), loc)), scale, now, loc)
}

// eightDone is a pace of two tasks a week: eight done in the last four weeks,
// and two done before them, which don't count.
func eightDone() []*dgraphStruct.DgraphTask {
	var out []*dgraphStruct.DgraphTask
	for i, d := range []int{2, 5, 9, 12, 16, 20, 26, 29} {
		out = append(out, done(i, oct(d)))
	}
	return append(out, done(8, at(time.UTC, 2026, 9, 20, 9)), done(9, oct(1)))
}

func TestTheForecastIsWhatIsOpenAtTheProjectsPace(t *testing.T) {
	tasks := append(eightDone(), open(1), open(2), open(3), open(4), open(5), open(6))
	got := buildFrom("p", tasks, nil, forecastNow, time.UTC)
	// Six open at two a week: three weeks from the 29th; at 2.4 and 1.6 a
	// week, 18 and 27 days.
	if got.State != ForecastLands || *got.Date != "2026-11-19" || *got.ByPace != "2026-11-19" || got.ByLinks != nil {
		t.Errorf("by its pace: %+v", got)
	}
	if *got.Optimistic != "2026-11-16" || *got.Pessimistic != "2026-11-25" || got.Open != 6 || got.PerWeek != 2 || got.Unit != "tasks" || got.Weeks != 4 {
		t.Errorf("its range and pace: %+v", got)
	}
	if got.HeldBy == nil || len(got.HeldBy) != 0 {
		t.Errorf("nothing holds it up, and that's an empty list: %+v", got.HeldBy)
	}
}

func TestTheForecastCountsAsTheProjectsScaleDoes(t *testing.T) {
	a, b := open(1), open(2)
	a.EstimatePoints = pts(3)
	x, y := done(1, oct(20)), done(2, oct(27))
	x.EstimatePoints = pts(5)
	tasks := []*dgraphStruct.DgraphTask{a, b, x, y}
	// A scale that counts a task with no points as one, as the burndown and
	// the velocity do: 4 points open, and 6 done in four weeks.
	got := buildFrom("p", tasks, &estimateScale.Scale{Kind: "fibonacci", CountUnestimated: true}, forecastNow, time.UTC)
	if got.Unit != "points" || got.Open != 4 || got.PerWeek != 1.5 {
		t.Errorf("a task with no points counts as one: %+v", got)
	}
	// One that counts it as nothing: 3 open, 5 done.
	if got := buildFrom("p", tasks, &estimateScale.Scale{Kind: "fibonacci"}, forecastNow, time.UTC); got.Unit != "points" || got.Open != 3 || got.PerWeek != 1.25 {
		t.Errorf("a task with no points counts as nothing: %+v", got)
	}
	// A project that estimates in hours counts tasks.
	if got := buildFrom("p", tasks, nil, forecastNow, time.UTC); got.Unit != "tasks" || got.Open != 2 || got.PerWeek != 0.5 {
		t.Errorf("in hours, tasks: %+v", got)
	}
	// Open work the scale counts as nothing is still open work: in tasks,
	// never done.
	a.EstimatePoints = nil
	if got := buildFrom("p", tasks, &estimateScale.Scale{Kind: "fibonacci"}, forecastNow, time.UTC); got.State == ForecastDone || got.Unit != "tasks" || got.Open != 2 {
		t.Errorf("open work counted as nothing: %+v", got)
	}
}

func TestAYoungProjectsPaceIsOverTheWeeksItHas(t *testing.T) {
	// Made on the 23rd, it's a week old on the 29th, with ten tasks done in
	// that week and five open: ten a week, over one week, not 2.5 over four.
	var tasks []*dgraphStruct.DgraphTask
	for i := 0; i < 10; i++ {
		tasks = append(tasks, done(i, oct(23+i%7)))
	}
	for i := 1; i <= 5; i++ {
		tasks = append(tasks, open(i))
	}
	for _, x := range tasks {
		x.Project.CreatedAt = oct(23)
	}
	got := buildFrom("p", tasks, nil, forecastNow, time.UTC)
	if got.PerWeek != 10 || got.Weeks != 1 || got.ByPace == nil || *got.ByPace != "2026-11-02" {
		t.Errorf("ten a week over its one week: %+v", got)
	}
	// Three weeks old, three weeks of pace: the ten over three weeks.
	for _, x := range tasks {
		x.Project.CreatedAt = oct(8)
	}
	if got := buildFrom("p", tasks, nil, forecastNow, time.UTC); got.Weeks != 3 || math.Abs(got.PerWeek-10.0/3) > 1e-9 {
		t.Errorf("three weeks old: %+v", got)
	}
	// Made long ago, or on no day the read knows: the whole four weeks.
	for _, x := range tasks {
		x.Project.CreatedAt = nil
	}
	if got := buildFrom("p", tasks, nil, forecastNow, time.UTC); got.Weeks != 4 || got.PerWeek != 2.5 {
		t.Errorf("no day it was made: %+v", got)
	}
}

// lateChain is a (due the 20th, 9 days late) ← b (due the 25th, 4 days late)
// ← c (the 1st to the 20th of November), all in p.
func lateChain() (a, b, c *dgraphStruct.DgraphTask) {
	a = inProject(waits("a0000000-0000-4000-8000-00000000000a", oct(15), oct(20)), "p")
	b = inProject(waits("a0000000-0000-4000-8000-00000000000b", oct(22), oct(25), a.Uuid), "p")
	c = inProject(waits("a0000000-0000-4000-8000-00000000000c", nov(1), nov(20), b.Uuid), "p")
	a.Name, b.Name, c.Name = "API review", "Copy", "Launch page"
	return a, b, c
}

func TestALateBlockerMovesTheForecastPastThePace(t *testing.T) {
	a, b, c := lateChain()
	got := buildFrom("p", append(eightDone(), a, b, c), nil, forecastNow, time.UTC)
	// The pace says the 9th (three open at two a week). But a ends today at
	// the earliest, so b starts on the 30th and ends 2 Nov, and c moves two
	// days, to end on the 22nd.
	if *got.ByPace != "2026-11-09" || *got.ByLinks != "2026-11-22" || *got.Date != "2026-11-22" {
		t.Fatalf("the links hold it to the 22nd: %+v", got)
	}
	want := []ForecastHold{{UUID: a.Uuid, Name: "API review", DaysLate: 9}, {UUID: b.Uuid, Name: "Copy", DaysLate: 4}}
	if !reflect.DeepEqual(got.HeldBy, want) {
		t.Errorf("held by a and b, latest first: %+v", got.HeldBy)
	}
	// Done, a holds nothing up: the links end with c, as planned.
	a.Status = dgraphStruct.TASK_STATUS_DONE
	if got := buildFrom("p", append(eightDone(), a, b, c), nil, forecastNow, time.UTC); *got.ByLinks != "2026-11-20" || *got.Date != "2026-11-20" || len(got.HeldBy) != 0 {
		t.Errorf("with a done, c ends as planned: %+v", got)
	}
}

func TestTheForecastSaysHowManyLateTasksHoldItUp(t *testing.T) {
	a, b, c := lateChain()
	d := inProject(waits("a0000000-0000-4000-8000-00000000000d", oct(22), oct(25), a.Uuid), "p")
	e := inProject(waits("a0000000-0000-4000-8000-00000000000e", oct(22), oct(25), a.Uuid), "p")
	c = inProject(waits(c.Uuid, nov(1), nov(20), b.Uuid, d.Uuid, e.Uuid), "p")
	got := buildFrom("p", append(eightDone(), a, b, c, d, e), nil, forecastNow, time.UTC)
	// a, nine days late, then b, d and e, four days late each and each on the
	// path to c: four hold the date up, and the first three are named.
	if got.HeldCount != 4 || len(got.HeldBy) != forecastHeld || got.HeldBy[0].UUID != a.Uuid {
		t.Errorf("four hold it up, three named: %d %+v", got.HeldCount, got.HeldBy)
	}
}

func TestAnotherProjectsLateTaskHoldsTheForecastByName(t *testing.T) {
	q := inProject(waits("b0000000-0000-4000-8000-00000000000q", oct(10), oct(19)), "q")
	q.Name = "Brand sign-off"
	c := inProject(waits("b0000000-0000-4000-8000-00000000000c", oct(30), nov(25), q.Uuid), "p")
	got := buildFrom("p", append(eightDone(), q, c), nil, forecastNow, time.UTC)
	// q ends today at the earliest, so c starts on the 30th, as planned: the
	// links end with c on the 25th, and q is on the path to it.
	if *got.Date != "2026-11-25" || len(got.HeldBy) != 1 || got.HeldBy[0] != (ForecastHold{UUID: q.Uuid, Name: "Brand sign-off", External: true, ProjectName: "Marketing", DaysLate: 10}) {
		t.Errorf("held by Marketing's sign-off: %+v", got)
	}
	if got.Open != 1 {
		t.Errorf("only Launch's own work counts as open: %+v", got)
	}
}

func TestAForecastHasNothingToSayWithoutWorkOrPace(t *testing.T) {
	if got := buildFrom("p", nil, nil, forecastNow, time.UTC); got.State != ForecastStarting || got.Date != nil {
		t.Errorf("no tasks: %+v", got)
	}
	if got := buildFrom("p", []*dgraphStruct.DgraphTask{open(1), done(1, at(time.UTC, 2026, 9, 1, 9))}, nil, forecastNow, time.UTC); got.State != ForecastStarting || got.Date != nil || got.Open != 1 || got.PerWeek != 0 {
		t.Errorf("open work, nothing done lately, no dates: %+v", got)
	}
	if got := buildFrom("p", []*dgraphStruct.DgraphTask{done(1, oct(28))}, nil, forecastNow, time.UTC); got.State != ForecastDone || got.Date != nil {
		t.Errorf("all done: %+v", got)
	}
	// Dates and no pace: the links alone.
	_, _, c := lateChain()
	c.BlockedBy = nil
	if got := buildFrom("p", []*dgraphStruct.DgraphTask{c}, nil, forecastNow, time.UTC); got.State != ForecastLands || *got.Date != "2026-11-20" || got.ByPace != nil || got.Optimistic != nil {
		t.Errorf("by its links alone: %+v", got)
	}
}

func TestTheForecastLeavesOutALateTaskTheReaderCantSee(t *testing.T) {
	q := inProject(waits("c0000000-0000-4000-8000-00000000000q", oct(10), oct(19)), "q")
	c := inProject(waits("c0000000-0000-4000-8000-00000000000c", oct(30), nov(2), q.Uuid), "p")
	stubPlan(t, append(eightDone(), q, c), nil)
	judge := shiftSeams.judge
	shiftSeams.judge = func(ctx context.Context, u, uid string, ts ...*dgraphStruct.DgraphTask) (map[string]workAccess.Verdict, error) {
		out, err := judge(ctx, u, uid, ts...)
		out[strings.ToLower(q.Uuid)] = workAccess.Verdict{}
		return out, err
	}
	got, err := Forecast(context.Background(), "p", forecastNow, time.UTC, &dgraphStruct.DgraphUser{Uid: "0x7", Uuid: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	// Unseen, q neither moves c nor is named: c ends on the 2nd, as planned,
	// and the pace (one open at two a week: the 2nd too) says the same.
	if *got.ByLinks != "2026-11-02" || len(got.HeldBy) != 0 || strings.Contains(fmt.Sprint(got), "Marketing") {
		t.Errorf("nothing of Marketing's: %+v", got)
	}
}

// forecastReads stubs every read many projects' forecasts make, each counted:
// who may see them (only the projects in may), their plans (each project's
// own open task, which it may see), Work Access's verdicts, the projects'
// counts (what the graph would count of those plans) and their estimate
// scales (none: hours). asked is the projects whose plans were read.
func forecastReads(t *testing.T, may func(id string) bool) (counts *[5]int, asked *[]string) {
	t.Helper()
	counts, asked = &[5]int{}, &[]string{}
	stubShift(t, nil, nil)
	oldView, oldPlan := forecastSeams, planSeams
	t.Cleanup(func() { forecastSeams, planSeams = oldView, oldPlan })
	forecastSeams.viewable = func(_ context.Context, _, _ string, ids []string) (map[string]bool, error) {
		counts[0]++
		out := map[string]bool{}
		for _, id := range ids {
			if may(id) {
				out[strings.ToLower(id)] = true
			}
		}
		return out, nil
	}
	plans := map[string][]*dgraphStruct.DgraphTask{}
	planSeams.read = func(_ context.Context, ids []string, _ string, _ domain.PlanLimits) (map[string]domain.PlanRead, error) {
		counts[1]++
		*asked = append(*asked, ids...)
		out := map[string]domain.PlanRead{}
		for _, id := range ids {
			plans[strings.ToLower(id)] = []*dgraphStruct.DgraphTask{inProject(waits(uuid.NewString(), nil, nil), id)}
			out[strings.ToLower(id)] = domain.PlanRead{Tasks: plans[strings.ToLower(id)]}
		}
		return out, nil
	}
	judge := shiftSeams.judge
	shiftSeams.judge = func(ctx context.Context, u, uid string, ts ...*dgraphStruct.DgraphTask) (map[string]workAccess.Verdict, error) {
		counts[2]++
		return judge(ctx, u, uid, ts...)
	}
	forecastSeams.totals = func(_ context.Context, ids []string, bounds []time.Time) (map[string]domain.PlanTotals, error) {
		counts[3]++
		out := map[string]domain.PlanTotals{}
		for _, id := range ids {
			out[strings.ToLower(id)] = totalsOf(id, plans[strings.ToLower(id)], bounds)
		}
		return out, nil
	}
	forecastSeams.scales = func(context.Context, []uuid.UUID) (map[uuid.UUID]*estimateScale.Scale, error) {
		counts[4]++
		return map[uuid.UUID]*estimateScale.Scale{}, nil
	}
	return counts, asked
}

// projectIDs is n project ids.
func projectIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("f0000000-0000-4000-8000-%012d", i)
	}
	return ids
}

func TestForecastsReadAsOftenForFiftyProjectsAsForOne(t *testing.T) {
	reads := map[int][5]int{}
	for _, n := range []int{1, 50} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			counts, _ := forecastReads(t, func(string) bool { return true })
			got, err := Forecasts(context.Background(), projectIDs(n), forecastNow, time.UTC, &dgraphStruct.DgraphUser{Uid: "0x7", Uuid: uuid.NewString()})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != n {
				t.Fatalf("%d forecasts for %d projects", len(got), n)
			}
			reads[n] = *counts
		})
	}
	// Who may see them, their plans, the verdicts, their counts and their
	// scales: one read each, for one project as for fifty.
	if reads[1] != [5]int{1, 1, 1, 1, 1} || reads[50] != reads[1] {
		t.Errorf("reads (who may see, plans, verdicts, counts, scales) for one project %v, for fifty %v: want [1 1 1 1 1] for both", reads[1], reads[50])
	}
}

func TestForecastsLeaveOutAProjectTheReaderCantSeeUnread(t *testing.T) {
	ids := projectIDs(3)
	_, asked := forecastReads(t, func(id string) bool { return id != ids[1] })
	got, err := Forecasts(context.Background(), ids, forecastNow, time.UTC, &dgraphStruct.DgraphUser{Uid: "0x7", Uuid: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[ids[1]]; ok || len(got) != 2 || got[ids[0]] == nil || got[ids[2]] == nil {
		t.Errorf("forecasts for %v: want %s and %s only", reflect.ValueOf(got).MapKeys(), ids[0], ids[2])
	}
	// The plan of a project they can't see is never read.
	for _, id := range *asked {
		if id == ids[1] {
			t.Errorf("read the plan of %s, which the reader can't see", id)
		}
	}
}

// A very large project's forecast (linear-20, main 29 Oct 14:40): the plan
// read stopped at its limit, so what is open and the pace come from the
// graph's totals, read for the reader (the same visibility the plan has).
func TestACutForecastCountsEveryTaskFromTheTotals(t *testing.T) {
	q := inProject(waits("c0000000-0000-4000-8000-00000000000q", oct(10), oct(19)), "q")
	c := inProject(waits("c0000000-0000-4000-8000-00000000000c", oct(30), nov(2), q.Uuid), "p")
	h := inProject(waits("c0000000-0000-4000-8000-0000000000aa", oct(30), nov(3)), "p")
	tasks := []*dgraphStruct.DgraphTask{done(1, oct(27)), done(2, oct(28)), q, c, h}
	stubPlan(t, tasks, nil)
	planSeams.read = planOf(func(string, domain.PlanLimits) ([]*dgraphStruct.DgraphTask, domain.PlanCut) {
		return tasks, domain.PlanCut{Finished: true}
	})
	judge := shiftSeams.judge
	shiftSeams.judge = func(ctx context.Context, u, uid string, ts ...*dgraphStruct.DgraphTask) (map[string]workAccess.Verdict, error) {
		out, err := judge(ctx, u, uid, ts...)
		out[strings.ToLower(h.Uuid)] = workAccess.Verdict{}
		return out, err
	}
	reader := &dgraphStruct.DgraphUser{Uid: "0x7", Uuid: uuid.NewString()}
	var asked string
	var spans []time.Time
	forecastSeams.totals = totalsFor(func(ctx context.Context, project string, bounds []time.Time) domain.PlanTotals {
		asked, spans = taskread.ReaderOf(ctx)+" "+project, bounds
		w := domain.Tally{N: 3}
		return domain.PlanTotals{Tasks: 40, Open: domain.Tally{N: 12}, Done: []domain.Tally{w, w, w, w}}
	})
	got, err := Forecast(context.Background(), "p", forecastNow, time.UTC, reader)
	if err != nil {
		t.Fatal(err)
	}
	if asked != reader.Uuid+" p" {
		t.Errorf("the totals are read for the reader of p, as %q", asked)
	}
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	if want := []time.Time{day(30), day(23), day(16), day(9), day(2)}; !reflect.DeepEqual(spans, want) {
		t.Errorf("the pace's four weeks, to the end of the 29th: %v, want %v", spans, want)
	}
	if got.Open != 12 || got.PerWeek != 3 || got.Unit != "tasks" {
		t.Errorf("open work and the pace from the totals, not the cut read: open %v, %v a week", got.Open, got.PerWeek)
	}
	if got.OpenCut != nil {
		t.Errorf("a read that left out only finished work says nothing of it: %+v", got.OpenCut)
	}
}

// Open tasks alone over the plan read's limit (linear-20, main 29 Oct 15:3x):
// the read kept some of the project's open tasks, so the answer says how many
// there are and how many of them the date's links count, both as the reader
// may see them: the project's open tasks from the totals, and those of them in
// the plan. Another project's work counts in neither. Only then: not when the
// plan holds every open task the reader may see, nor for a cut of finished
// work alone.
func TestAForecastWhoseOpenWorkWasCutSaysHowMuchItsLinksCount(t *testing.T) {
	q := inProject(waits("c0000000-0000-4000-8000-00000000000q", oct(10), oct(19)), "q")
	c := inProject(waits("c0000000-0000-4000-8000-00000000000c", oct(30), nov(2), q.Uuid), "p")
	h := inProject(waits("c0000000-0000-4000-8000-0000000000aa", oct(30), nov(3)), "p")
	tasks := []*dgraphStruct.DgraphTask{done(1, oct(27)), q, c, h, open(2)}
	stubPlan(t, tasks, nil)
	cut := domain.PlanCut{Open: true}
	planSeams.read = planOf(func(string, domain.PlanLimits) ([]*dgraphStruct.DgraphTask, domain.PlanCut) {
		return tasks, cut
	})
	// The reader may not see h.
	judge := shiftSeams.judge
	shiftSeams.judge = func(ctx context.Context, u, uid string, ts ...*dgraphStruct.DgraphTask) (map[string]workAccess.Verdict, error) {
		out, err := judge(ctx, u, uid, ts...)
		out[strings.ToLower(h.Uuid)] = workAccess.Verdict{}
		return out, err
	}
	openN := 12
	forecastSeams.totals = totalsFor(func(context.Context, string, []time.Time) domain.PlanTotals {
		w := domain.Tally{N: 3}
		return domain.PlanTotals{Tasks: 40, Open: domain.Tally{N: openN}, Done: []domain.Tally{w, w, w, w}}
	})
	reader := &dgraphStruct.DgraphUser{Uid: "0x7", Uuid: uuid.NewString()}
	forecast := func() *ForecastAnswer {
		t.Helper()
		got, err := Forecast(context.Background(), "p", forecastNow, time.UTC, reader)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := forecast(); got.OpenCut == nil || *got.OpenCut != (ForecastOpenCut{Total: 12, Counted: 2}) || got.Open != 12 {
		t.Errorf("of p's 12 open tasks, the links count c and the undated one (not q, Marketing's, nor h, unseen); open work counts all 12: %+v, open %v", got.OpenCut, got.Open)
	}
	openN = 2
	if got := forecast(); got.OpenCut != nil {
		t.Errorf("the plan holds every open task the reader may see, so nothing was left out for them: %+v", got.OpenCut)
	}
	cut, openN = domain.PlanCut{Finished: true}, 12
	if got := forecast(); got.OpenCut != nil {
		t.Errorf("a cut of finished work alone says nothing: %+v", got.OpenCut)
	}
}

// A plan that sums a project up, its critical path or its forecast, reads at
// most planLimit of the project's own open tasks and of its finished ones,
// and the critical path says when the read left something out (linear-20). A
// move's impact reads every open task: its finish before and after, and the
// work it pushes along, must be what the move would write.
func TestASummaryReadsUpToTheLimitAndAMovesImpactEveryOpenTask(t *testing.T) {
	loc := time.UTC
	a, b, c, d := chainOf(loc)
	stubPlan(t, []*dgraphStruct.DgraphTask{a, b, c, d}, nil)
	var asked []domain.PlanLimits
	planSeams.read = planOf(func(_ string, limits domain.PlanLimits) ([]*dgraphStruct.DgraphTask, domain.PlanCut) {
		asked = append(asked, limits)
		return []*dgraphStruct.DgraphTask{a, b, c, d}, domain.PlanCut{Open: true}
	})
	reader := &dgraphStruct.DgraphUser{Uid: "0x7", Uuid: uuid.NewString()}
	got, err := CriticalPath(context.Background(), "p", loc, reader)
	if err != nil || !got.Truncated {
		t.Errorf("a critical path whose read left open work out says it was cut: %+v %v", got, err)
	}
	due := time.Date(2026, 10, 12, 9, 0, 0, 0, loc)
	if _, err := Impact(context.Background(), a, TaskDates{Due: &due}, loc, reader); err != nil {
		t.Fatal(err)
	}
	if _, err := Forecast(context.Background(), "p", forecastNow, loc, reader); err != nil {
		t.Fatal(err)
	}
	summary := domain.PlanLimits{Open: planLimit, Finished: planLimit}
	if want := []domain.PlanLimits{summary, {Finished: planLimit}, summary}; !reflect.DeepEqual(asked, want) {
		t.Errorf("the critical path, a move's impact and the forecast read %+v, want %+v", asked, want)
	}
}

// The pace's weeks are the reader's: in Kolkata, 29 Oct starts at 18:30 UTC
// on the 28th; across a change of the clocks, each week still starts at local
// midnight.
func TestThePacesWeeksAreTheReadersDays(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skip(err)
	}
	b := paceBounds(topo.CivilDay(forecastNow, kolkata), kolkata)
	if len(b) != forecastWeeks+1 || !b[0].Equal(time.Date(2026, 10, 29, 18, 30, 0, 0, time.UTC)) || !b[4].Equal(time.Date(2026, 10, 1, 18, 30, 0, 0, time.UTC)) {
		t.Errorf("Kolkata's weeks: %v", b)
	}
	york, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	// New York leaves daylight time on 1 Nov 2026.
	for _, s := range paceBounds(topo.CivilDay(time.Date(2026, 11, 20, 12, 0, 0, 0, york), york), york) {
		if l := s.In(york); l.Hour() != 0 || l.Minute() != 0 {
			t.Errorf("a week starts at %v in New York", l)
		}
	}
}

// What is open and the pace count every task the totals hold: on a scale,
// a task with no points counts as the scale says.
func TestTheForecastCountsFromTheTotals(t *testing.T) {
	totals := domain.PlanTotals{Tasks: 9, Open: domain.Tally{N: 3, Pointed: 1, Points: 3}, Done: []domain.Tally{{N: 1, Pointed: 1, Points: 2}, {N: 1}, {}, {}}}
	got := BuildForecast("p", nil, totals, nil, forecastNow, time.UTC)
	if got.Unit != "tasks" || got.Open != 3 || got.PerWeek != 0.5 || got.State != ForecastLands {
		t.Errorf("in tasks: %+v", got)
	}
	got = BuildForecast("p", nil, totals, &estimateScale.Scale{Kind: "fibonacci", CountUnestimated: true}, forecastNow, time.UTC)
	if got.Unit != "points" || got.Open != 5 || got.PerWeek != 0.75 {
		t.Errorf("in points: %+v", got)
	}
	// Nothing open of nine tasks: done.
	if got := BuildForecast("p", nil, domain.PlanTotals{Tasks: 9, Done: make([]domain.Tally, 4)}, nil, forecastNow, time.UTC); got.State != ForecastDone {
		t.Errorf("all done: %+v", got)
	}
}
