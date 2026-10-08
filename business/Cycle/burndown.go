package business

import (
	"context"
	"math"
	"sort"
	"time"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

// Burndown and velocity: how a cycle's open work went down day by day, and
// how much the project's recent cycles finished. Both count tasks and, where
// tasks have estimates, hours.

// VelocityCycles is how many completed cycles velocity looks back over, and
// TypicalCycles how many of the latest it averages.
const (
	VelocityCycles = 6
	TypicalCycles  = 3
	// maxDays bounds a cycle's days: eight weeks, and a day either side for a
	// reader zones away.
	maxDays = 8*7 + 2
)

// TaskProgress is what burndown and velocity need of a task.
type TaskProgress struct {
	Status string
	// Since is when the status was set; nil when it never changed.
	Since *time.Time
	// Hours is the estimate; HasEstimate says whether there is one.
	Hours       float64
	HasEstimate bool
}

func progressOf(t *dgraphStruct.DgraphTask) TaskProgress {
	p := TaskProgress{Status: t.Status, Since: t.StatusSince}
	if t.EstimateMinutes != nil && *t.EstimateMinutes > 0 {
		p.Hours, p.HasEstimate = float64(*t.EstimateMinutes)/60, true
	}
	return p
}

// Burndown is a cycle's work by day, in the reader's zone. Scope and
// Remaining run only up to today, or to the day the cycle was completed;
// Ideal is an even pace from the first day's scope to nothing by the end of
// the last day. The hour series are null unless some task has an estimate.
type Burndown struct {
	Days           []string  `json:"days"`
	Scope          []int     `json:"scope"`
	Remaining      []int     `json:"remaining"`
	Ideal          []float64 `json:"ideal"`
	ScopeHours     []float64 `json:"scope_hours"`
	RemainingHours []float64 `json:"remaining_hours"`
	IdealHours     []float64 `json:"ideal_hours"`
	// Tasks in the cycle, Done of them (cancelled ones aren't), Open still to
	// do, and Estimated with an estimate; Hours is their estimates added up.
	Tasks     int     `json:"tasks"`
	Done      int     `json:"done"`
	Open      int     `json:"open"`
	Estimated int     `json:"estimated"`
	Hours     float64 `json:"hours"`
}

func closedStatus(s string) bool { return !isOpen(s) }

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// BuildBurndown counts a cycle's work by day. A task counts from the day it
// joined (from the first day if it joined before the cycle began) and stops
// being remaining the day it was finished or cancelled. A task the cycle left
// unfinished when it was completed stays remaining to the end, whatever it
// became later. Tasks not in tasks (archived) aren't counted. Pure.
func BuildBurndown(c cycleModel.Cycle, members []cycleModel.Member, tasks map[string]TaskProgress, now time.Time, loc *time.Location) Burndown {
	b := Burndown{Days: []string{}, Scope: []int{}, Remaining: []int{}, Ideal: []float64{}}
	// The reader's days the cycle touches, from the one it starts on to the
	// one it ends in: a reader zones away from whoever made it can see one
	// more. Its end is its start plus whole days of 24 hours, which a clock
	// change can move an hour into the next day; the hour taken off keeps that
	// out. Days are counted on the calendar at noon, so a zone whose clocks
	// change at midnight still names each day once.
	y, m, d := c.StartsAt.In(loc).Date()
	ly, lm, ld := c.EndsAt.Add(-time.Hour - time.Nanosecond).In(loc).Date()
	last := time.Date(ly, lm, ld, 12, 0, 0, 0, loc)
	var starts, ends []time.Time
	for i := 0; i < maxDays; i++ {
		noon := time.Date(y, m, d+i, 12, 0, 0, 0, loc)
		if i > 0 && noon.After(last) {
			break
		}
		b.Days = append(b.Days, noon.Format(time.DateOnly))
		starts = append(starts, helpers.DayStart(noon, loc))
		ends = append(ends, helpers.DayStart(time.Date(y, m, d+i+1, 12, 0, 0, 0, loc), loc))
	}
	// What has happened so far: up to now, or to when the cycle was completed.
	until := now
	if c.CompletedAt != nil && c.CompletedAt.Before(until) {
		until = *c.CompletedAt
	}
	known := 0
	for i := range starts {
		if !starts[i].After(until) {
			known = i + 1
		}
	}
	scope, remaining := make([]int, known), make([]int, known)
	scopeH, remainingH := make([]float64, known), make([]float64, known)
	var totalH float64
	for _, m := range members {
		t, ok := tasks[m.TaskUUID]
		if !ok {
			continue
		}
		var closedAt *time.Time
		if !m.Unfinished && closedStatus(t.Status) {
			closedAt = t.Since
			if closedAt == nil { // closed with no record of when: since it joined
				closedAt = &m.AddedAt
			}
		}
		b.Tasks++
		totalH += t.Hours
		if t.HasEstimate {
			b.Estimated++
		}
		if closedAt == nil {
			b.Open++
		} else if t.Status == dgraphStruct.TASK_STATUS_DONE {
			b.Done++
		}
		for i := 0; i < known; i++ {
			if !m.AddedAt.Before(ends[i]) {
				continue
			}
			scope[i]++
			scopeH[i] += t.Hours
			if closedAt == nil || !closedAt.Before(ends[i]) {
				remaining[i]++
				remainingH[i] += t.Hours
			}
		}
	}
	b.Scope, b.Remaining = scope, remaining
	// The ideal paces what was in by the end of the first day (or, before the
	// cycle starts, everything in it now) evenly down to nothing.
	from, fromH := float64(b.Tasks), totalH
	if known > 0 {
		from, fromH = float64(scope[0]), scopeH[0]
	}
	b.Ideal = idealLine(from, len(b.Days))
	b.Hours = round1(totalH)
	if b.Estimated > 0 {
		b.ScopeHours, b.RemainingHours = make([]float64, known), make([]float64, known)
		for i := 0; i < known; i++ {
			b.ScopeHours[i], b.RemainingHours[i] = round1(scopeH[i]), round1(remainingH[i])
		}
		b.IdealHours = idealLine(fromH, len(b.Days))
	}
	return b
}

// idealLine is what an even pace leaves at the end of each of n days: a
// share of start done each day, none left by the end of the last.
func idealLine(start float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = round1(start * float64(n-1-i) / float64(n))
	}
	return out
}

// CycleDone is what a completed cycle finished, and how much it left.
type CycleDone struct {
	Id         uuid.UUID `json:"id"`
	Number     int       `json:"number"`
	Name       string    `json:"name"`
	Done       int       `json:"done"`
	DoneHours  float64   `json:"done_hours"`
	Unfinished int       `json:"unfinished"`
	// estimated: some of what it finished had an estimate.
	estimated bool
}

// Velocity is the latest completed cycles, oldest first, with what the most
// recent ones finished on average (Typical, in tasks and hours).
type Velocity struct {
	Cycles       []CycleDone `json:"cycles"`
	Typical      *float64    `json:"typical,omitempty"`
	TypicalHours *float64    `json:"typical_hours,omitempty"`
}

// BuildVelocity counts what each of the last completed cycles finished: its
// tasks that are done (a cancelled one isn't). Pure.
func BuildVelocity(cycles []cycleModel.Cycle, members map[uuid.UUID][]string, tasks map[string]TaskProgress) Velocity {
	completed := completedCycles(cycles)
	v := Velocity{Cycles: []CycleDone{}}
	for _, c := range completed {
		d := CycleDone{Id: c.Id, Number: c.Number, Name: c.Name}
		if c.CarriedCount != nil {
			d.Unfinished = *c.CarriedCount
		}
		for _, id := range members[c.Id] {
			if t, ok := tasks[id]; ok && t.Status == dgraphStruct.TASK_STATUS_DONE {
				d.Done++
				d.DoneHours += t.Hours
				d.estimated = d.estimated || t.HasEstimate
			}
		}
		d.DoneHours = round1(d.DoneHours)
		v.Cycles = append(v.Cycles, d)
	}
	if n := min(len(v.Cycles), TypicalCycles); n > 0 {
		var tasksSum, hoursSum float64
		// Hours only when the cycles averaged counted some.
		anyHours := false
		for _, d := range v.Cycles[len(v.Cycles)-n:] {
			tasksSum += float64(d.Done)
			hoursSum += d.DoneHours
			anyHours = anyHours || d.estimated
		}
		typical := round1(tasksSum / float64(n))
		v.Typical = &typical
		if anyHours {
			typicalH := round1(hoursSum / float64(n))
			v.TypicalHours = &typicalH
		}
	}
	return v
}

// completedCycles is the project's latest completed cycles, oldest first.
func completedCycles(cycles []cycleModel.Cycle) []cycleModel.Cycle {
	var out []cycleModel.Cycle
	for _, c := range cycles {
		if c.CompletedAt != nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	if len(out) > VelocityCycles {
		out = out[len(out)-VelocityCycles:]
	}
	return out
}

// BurndownView is one cycle, its burndown and the project's velocity.
type BurndownView struct {
	Cycle    View     `json:"cycle"`
	Burndown Burndown `json:"burndown"`
	Velocity Velocity `json:"velocity"`
}

// GetBurndown reads a cycle's burndown, in loc's days, and the velocity of
// its project's latest completed cycles.
func GetBurndown(ctx context.Context, c *cycleModel.Cycle, now time.Time, loc *time.Location) (*BurndownView, error) {
	members, err := cycleModel.MembersOf(c.Id)
	if err != nil {
		return nil, err
	}
	cycles, err := cycleModel.List(c.ProjectUUID)
	if err != nil {
		return nil, err
	}
	byCycle, err := cycleModel.Members(c.ProjectUUID)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, m := range members {
		ids[m.TaskUUID] = true
	}
	for _, done := range completedCycles(cycles) {
		for _, id := range byCycle[done.Id] {
			ids[id] = true
		}
	}
	all := make([]string, 0, len(ids))
	for id := range ids {
		all = append(all, id)
	}
	found, err := taskDomain.GetDgraphTaskStatuses(ctx, all)
	if err != nil {
		return nil, err
	}
	tasks := make(map[string]TaskProgress, len(found))
	for _, t := range found {
		tasks[t.Uuid] = progressOf(t)
	}
	// The cycle's progress, counted as the cycles list counts it.
	var statuses []string
	for _, id := range byCycle[c.Id] {
		if t, ok := tasks[id]; ok {
			statuses = append(statuses, t.Status)
		}
	}
	return &BurndownView{
		Cycle:    View{Cycle: *c, Progress: Count(statuses), State: State(*c, now)},
		Burndown: BuildBurndown(*c, members, tasks, now, loc),
		Velocity: BuildVelocity(cycles, byCycle, tasks),
	}, nil
}
