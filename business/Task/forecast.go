package business

// A project's forecast (linear-20): when its open work lands, from how fast
// the project gets work done and from what that work waits on.
//
// The pace is the project's own tasks done in the last four weeks (or in the
// weeks a younger project has had), a week at a time, counted as its
// estimate scale counts them; what is open over that pace, from today, gives
// a date, with an optimistic and a pessimistic pace 20% either side (their
// dates about 40% apart, as Linear's project graph draws them).
//
// The links give a floor the pace can't see. Open work can't end before
// today, so a late task ends today at the earliest, keeping its length, and
// pushes the work waiting on it along the links, as moving it would
// (helpers/topo: Shift, then CriticalPath). The project's open work ends no
// earlier than that. The forecast is the later of the two dates, and names
// the late tasks on the critical path that hold it up.
//
// What is open and the pace come from the graph's counts of the project's
// tasks (domain.ReadPlanTotalsOf), every one the member may see, however many.
// The links come from one plan, read as the critical path reads it
// (readPlan): only tasks the member may see take part, so nothing it says
// speaks of work they can't see. On a very large project that plan keeps at
// most planLimit of the project's own open tasks, the soonest due first and
// then the undated, the earliest made first, and as many finished ones, the
// latest finished first (summaryLimits). A cut of finished work alone moves
// nothing (finished work neither moves nor holds up another), so the forecast
// is whole and says nothing of it. Open tasks over the limit leave the latest
// due out of the links: then OpenCut says how many open tasks the links
// count, while the pace and the open work still count every one.

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	estimateScale "github.com/akashc777/OneCamp/business/EstimateScale"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers/topo"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

const (
	// forecastWeeks is how many weeks of done work the pace reads.
	forecastWeeks = 4
	// forecastSpread is how far the optimistic and pessimistic paces sit
	// either side of the pace.
	forecastSpread = 0.2
	// forecastHeld is how many late tasks the forecast names at most.
	forecastHeld = 3
)

// The states of a forecast.
const (
	// ForecastLands is a forecast with a date.
	ForecastLands = "forecast"
	// ForecastDone is a project with tasks, all of them finished.
	ForecastDone = "done"
	// ForecastStarting is a project with nothing to go on yet: no tasks, or
	// open work with nothing done in the last four weeks and no dates.
	ForecastStarting = "starting"
)

// ForecastHold is a late task on the critical path: open past its due day.
type ForecastHold struct {
	UUID        string `json:"task_uuid"`
	Name        string `json:"task_name"`
	External    bool   `json:"external"`
	ProjectName string `json:"project_name,omitempty"`
	DaysLate    int    `json:"days_late"`
}

// ForecastAnswer is GET /project/{id}/forecast's answer. Dates are days
// (YYYY-MM-DD) in the reader's zone.
type ForecastAnswer struct {
	ProjectUUID string `json:"project_uuid"`
	State       string `json:"state"`
	// Date is the later of ByPace and ByLinks.
	Date        *string `json:"date"`
	Optimistic  *string `json:"optimistic"`
	Pessimistic *string `json:"pessimistic"`
	ByPace      *string `json:"by_pace"`
	ByLinks     *string `json:"by_links"`
	// Open is what is left, in Unit: "points" as the project's estimate
	// scale counts them, else "tasks" (a project in hours, or open work the
	// scale counts as nothing).
	Open    float64 `json:"open"`
	Unit    string  `json:"unit"`
	PerWeek float64 `json:"per_week"`
	// Weeks is how many weeks the pace read: four, or fewer for a younger
	// project.
	Weeks int `json:"weeks"`
	// HeldBy are the late tasks that hold the date up, latest first: the
	// first forecastHeld of them.
	HeldBy []ForecastHold `json:"held_by"`
	// HeldCount is how many late tasks hold the date up, named or not.
	HeldCount int `json:"held_count"`
	// OpenCut is there only when the project has more open tasks than one
	// plan read keeps, so the date's links count some of them; its pace and
	// open work still count every one.
	OpenCut *ForecastOpenCut `json:"open_cut,omitempty"`
}

// ForecastOpenCut is how many open tasks the project has, as its reader may
// see them, and how many of them the date's links count: the soonest due,
// then the earliest made.
type ForecastOpenCut struct {
	Total   int `json:"total"`
	Counted int `json:"counted"`
}

// Forecast is projectUUID's forecast as user may see it, on now's day in loc.
func Forecast(ctx context.Context, projectUUID string, now time.Time, loc *time.Location, user *dgraphStruct.DgraphUser) (*ForecastAnswer, error) {
	ctx = taskread.ActingFor(ctx, user.Uuid)
	id := strings.ToLower(projectUUID)
	out, err := forecastsOf(ctx, []string{id}, now, loc, user)
	if err != nil {
		return nil, err
	}
	return out[id], nil
}

// forecastSeams are where forecasts ask who may see many projects, and read
// the projects' counts and estimate scales: seams for tests.
var forecastSeams = struct {
	viewable func(ctx context.Context, userUUID, userUID string, projectUUIDs []string) (map[string]bool, error)
	totals   func(ctx context.Context, projectUUIDs []string, bounds []time.Time) (map[string]domain.PlanTotals, error)
	scales   func(ctx context.Context, projects []uuid.UUID) (map[uuid.UUID]*estimateScale.Scale, error)
}{viewable: workAccess.ProjectsViewable, totals: domain.ReadPlanTotalsOf, scales: estimateScale.OfMany}

// Forecasts is Forecast for each of projectUUIDs that user may see, by
// lower-case project id, in the same five reads however many there are: who
// may see them, their plans, Work Access's verdicts on the plans' tasks, their
// counts, and their estimate scales. Projects user can't see, and ids that
// name none, are left out, and nothing more of them is read.
func Forecasts(ctx context.Context, projectUUIDs []string, now time.Time, loc *time.Location, user *dgraphStruct.DgraphUser) (map[string]*ForecastAnswer, error) {
	out := map[string]*ForecastAnswer{}
	if len(projectUUIDs) == 0 {
		return out, nil
	}
	ctx = taskread.ActingFor(ctx, user.Uuid)
	may, err := forecastSeams.viewable(ctx, user.Uuid, user.Uid, projectUUIDs)
	if err != nil {
		return nil, err
	}
	var seen []string
	picked := map[string]bool{}
	for _, id := range projectUUIDs {
		if id = strings.ToLower(id); may[id] && !picked[id] {
			picked[id] = true
			seen = append(seen, id)
		}
	}
	if len(seen) == 0 {
		return out, nil
	}
	return forecastsOf(ctx, seen, now, loc, user)
}

// forecastsOf is the forecast of each of ids (lower-case, each once), by id:
// one read of their plans as a summary keeps them, one of Work Access's
// verdicts, one of their counts and one of their estimate scales.
func forecastsOf(ctx context.Context, ids []string, now time.Time, loc *time.Location, user *dgraphStruct.DgraphUser) (map[string]*ForecastAnswer, error) {
	plans, err := readPlans(ctx, ids, user, summaryLimits())
	if err != nil {
		return nil, err
	}
	totals, err := forecastSeams.totals(ctx, ids, paceBounds(topo.CivilDay(now, loc), loc))
	if err != nil {
		return nil, err
	}
	scales, err := scalesOf(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*ForecastAnswer, len(ids))
	for _, id := range ids {
		out[id] = forecastOf(id, plans[id], totals[id], scales[id], now, loc)
	}
	return out, nil
}

// forecastOf is the forecast of projectUUID from its plan p and its counts,
// on its estimate scale, on now's day in loc, with OpenCut when the plan's
// read left some of its open tasks out. Pure.
func forecastOf(projectUUID string, p plan, totals domain.PlanTotals, scale *estimateScale.Scale, now time.Time, loc *time.Location) *ForecastAnswer {
	out := BuildForecast(projectUUID, p.visible, totals, scale, now, loc)
	if p.cut.Open {
		out.OpenCut = openCutOf(projectUUID, p.visible, totals.Open.N)
	}
	return &out
}

// openCutOf is a forecast's OpenCut when its plan read left some of the
// project's open tasks out: open, how many the reader may see (the totals),
// and how many of them tasks, the plan the reader may see, holds. Nil when it
// holds every one, since then the links counted them all.
func openCutOf(projectUUID string, tasks []*dgraphStruct.DgraphTask, open int) *ForecastOpenCut {
	counted := 0
	for _, t := range tasks {
		if ofProject(t, projectUUID) && !closed(t) {
			counted++
		}
	}
	if counted >= open {
		return nil
	}
	return &ForecastOpenCut{Total: open, Counted: counted}
}

// paceBounds are the weeks the pace may read, to today in loc, latest first:
// the instants that start and end forecastWeeks weeks, the first ending with
// today (domain.ReadPlanTotalsOf's bounds).
func paceBounds(today topo.Day, loc *time.Location) []time.Time {
	out := make([]time.Time, forecastWeeks+1)
	for i := range out {
		out[i] = (today + 1 - topo.Day(7*i)).Start(loc)
	}
	return out
}

// scalesOf is each of projectUUIDs' estimate scale, by lower-case id, in one
// read. A project that estimates in hours, or an id that isn't one, has none.
func scalesOf(ctx context.Context, projectUUIDs []string) (map[string]*estimateScale.Scale, error) {
	var ids []uuid.UUID
	for _, p := range projectUUIDs {
		if id, err := uuid.Parse(p); err == nil {
			ids = append(ids, id)
		}
	}
	read, err := forecastSeams.scales(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*estimateScale.Scale, len(read))
	for id, s := range read {
		out[id.String()] = s
	}
	return out, nil
}

// BuildForecast is projectUUID's forecast on now's day in loc: what is open
// and the pace from totals (its tasks, every one: domain.ReadPlanTotalsOf over
// paceBounds), counted as the project's estimate scale counts them (nil: it
// estimates in hours), and the links from tasks (the project's and those
// linked to them, as readPlan keeps them). Pure.
func BuildForecast(projectUUID string, tasks []*dgraphStruct.DgraphTask, totals domain.PlanTotals, scale *estimateScale.Scale, now time.Time, loc *time.Location) ForecastAnswer {
	out := ForecastAnswer{ProjectUUID: projectUUID, State: ForecastStarting, Unit: "tasks", HeldBy: []ForecastHold{}}
	today := topo.CivilDay(now, loc)
	out.Weeks = paceWeeks(totals, today, loc)
	open, perWeek, unit := paceOf(totals, scale, out.Weeks)
	out.Open, out.PerWeek, out.Unit = open, perWeek, unit
	if totals.Tasks > 0 && open == 0 {
		out.State = ForecastDone
		return out
	}
	day := func(d topo.Day) *string { s := d.String(); return &s }
	var byPace *topo.Day
	if perWeek > 0 {
		at := func(pace float64) topo.Day { return today + topo.Day(math.Ceil(open/pace*7)) }
		d := at(perWeek)
		byPace = &d
		out.ByPace = day(d)
		out.Optimistic = day(at(perWeek * (1 + forecastSpread)))
		out.Pessimistic = day(at(perWeek * (1 - forecastSpread)))
	}
	byLinks, held := linksFinish(projectUUID, tasks, today, loc)
	if byLinks != nil {
		out.ByLinks = day(*byLinks)
	}
	switch {
	case byPace == nil && byLinks == nil:
		return out
	case byLinks == nil || (byPace != nil && *byPace >= *byLinks):
		out.Date = out.ByPace
	default:
		out.Date = out.ByLinks
		out.HeldBy, out.HeldCount = held[:min(len(held), forecastHeld)], len(held)
	}
	out.State = ForecastLands
	return out
}

// paceWeeks is how many weeks the pace reads: forecastWeeks, or the whole
// weeks a younger project has had since its first day (at least one), so its
// done work isn't spread over weeks it didn't have. Its first day is the
// earlier of the day it was made and the first day it got a task done; with
// no day it was made known, the whole forecastWeeks.
func paceWeeks(t domain.PlanTotals, today topo.Day, loc *time.Location) int {
	if t.CreatedAt == nil {
		return forecastWeeks
	}
	first := topo.CivilDay(*t.CreatedAt, loc)
	if t.FirstDone != nil {
		first = min(first, topo.CivilDay(*t.FirstDone, loc))
	}
	return min(forecastWeeks, max(1, int(today-first+1)/7))
}

// paceOf is what is open of t and how much of it gets done a week, over its
// first weeks weeks of done work (the latest): in points as scale counts them
// (a task with none as one when it counts those, else as nothing; burndown
// and velocity count the same), else in tasks. A project that estimates in
// hours (no scale), or whose open work counts no points, goes by tasks.
func paceOf(t domain.PlanTotals, scale *estimateScale.Scale, weeks int) (open, perWeek float64, unit string) {
	var done domain.Tally
	for _, w := range t.Done[:min(weeks, len(t.Done))] {
		done.N, done.Pointed, done.Points = done.N+w.N, done.Pointed+w.Pointed, done.Points+w.Points
	}
	if scale != nil {
		if points := estimateScale.CountedAll(scale, t.Open.N, t.Open.Pointed, t.Open.Points); points > 0 {
			return points, estimateScale.CountedAll(scale, done.N, done.Pointed, done.Points) / float64(weeks), "points"
		}
	}
	return float64(t.Open.N), float64(done.N) / float64(weeks), "tasks"
}

// linksFinish is the last day of the project's open, dated work once every
// late task (open, its last day before today) ends today at the earliest and
// pushes what waits on it, and the late tasks then on the critical path.
// Only the project's tasks and the work they wait on take part. Nil when the
// project has no open, dated work.
func linksFinish(projectUUID string, tasks []*dgraphStruct.DgraphTask, today topo.Day, loc *time.Location) (*topo.Day, []ForecastHold) {
	s, byID := scheduleOf(tasks, loc)
	preds := map[string][]string{}
	for _, l := range s.Links {
		preds[l.To] = append(preds[l.To], l.From)
	}
	mine := func(id string) bool { return ofProject(byID[id], projectUUID) }
	// The project's tasks and everything they wait on, however far.
	keep := map[string]bool{}
	var queue []string
	for _, t := range s.Tasks {
		if mine(t.ID) {
			keep[t.ID] = true
			queue = append(queue, t.ID)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, p := range preds[id] {
			if !keep[p] {
				keep[p] = true
				queue = append(queue, p)
			}
		}
	}
	var u topo.Schedule
	at := map[string]int{}
	late := map[string]int{}
	for _, t := range s.Tasks {
		if !keep[t.ID] {
			continue
		}
		if !t.Closed && t.Dated && t.End < today {
			d := today - t.End
			t.Start, t.End = t.Start+d, t.End+d
			late[t.ID] = int(d)
		}
		at[t.ID] = len(u.Tasks)
		u.Tasks = append(u.Tasks, t)
	}
	for _, l := range s.Links {
		if keep[l.From] && keep[l.To] {
			u.Links = append(u.Links, l)
		}
	}
	// Each late task pushes what waits on it, upstream first, so a task
	// waiting on two late ones moves for both.
	ids := make([]string, len(u.Tasks))
	for i, t := range u.Tasks {
		ids[i] = t.ID
	}
	ordered, _ := topo.Order(ids, func(id string) []string { return preds[id] })
	for _, id := range ordered {
		if late[id] == 0 {
			continue
		}
		for _, m := range topo.Shift(u, id, len(u.Tasks)) {
			t := &u.Tasks[at[m.ID]]
			t.Start, t.End = t.Start+topo.Day(m.Days), t.End+topo.Day(m.Days)
		}
	}
	var finish *topo.Day
	for _, t := range u.Tasks {
		if mine(t.ID) && !t.Closed && t.Dated && (finish == nil || t.End > *finish) {
			end := t.End
			finish = &end
		}
	}
	if finish == nil {
		return nil, nil
	}
	plan, _ := topo.CriticalPath(u)
	held := []ForecastHold{}
	for id, days := range late {
		if slot, ok := plan.Slots[id]; !ok || !slot.Critical {
			continue
		}
		t := byID[id]
		h := ForecastHold{UUID: t.Uuid, Name: t.Name, External: !mine(id), DaysLate: days}
		if h.External && t.Project != nil {
			h.ProjectName = t.Project.Name
		}
		held = append(held, h)
	}
	sort.Slice(held, func(i, j int) bool {
		if held[i].DaysLate != held[j].DaysLate {
			return held[i].DaysLate > held[j].DaysLate
		}
		return held[i].UUID < held[j].UUID
	})
	return finish, held
}
