package business

import (
	"sort"
	"time"

	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

// The Cycle Scheduler (B3b; PROGRAM.md D-PJ-6, #93): a project's cycles run
// themselves. A schedule repeats every N weeks (1 to 8) from its first start,
// with an optional cooldown week between cycles; it keeps the current cycle
// and the next few (two unless the project says otherwise, at most fifteen)
// ready, and completes a cycle when it ends, rolling its unfinished tasks
// into the next one unless the project turned that off. The Sweeper runs it
// every 15 minutes (projects.cycles); saving a schedule runs it at once.

// MaxUpcoming is the most cycles a schedule keeps ready after the current one.
const MaxUpcoming = 15

// Schedule is a project's (cycleModel.Schedule as the planner reads it).
type Schedule struct {
	ProjectUUID uuid.UUID
	// Weeks is each cycle's length, 1 to 8.
	Weeks int
	// CooldownWeeks is the gap between one cycle's end and the next's start, 0 or 1.
	CooldownWeeks int
	// Upcoming is how many cycles after the current one are kept ready.
	Upcoming int
	// RollOver moves a completed cycle's unfinished tasks into the next one.
	RollOver bool
	// Anchor is the first cycle's start; every cycle starts a whole number
	// of periods after it.
	Anchor time.Time
	// By is who set the schedule; the cycles it makes are made by them.
	By uuid.UUID
}

// Slot is a cycle the schedule would make.
type Slot struct {
	StartsAt time.Time
	EndsAt   time.Time
}

// SchedulePlan is what a pass does to one project: make Create, in order,
// then complete Complete (ended cycles nobody completed), oldest first.
type SchedulePlan struct {
	Create   []Slot
	Complete []uuid.UUID
}

func (s Schedule) periodDays() int { return 7 * (s.Weeks + s.CooldownWeeks) }

// slot is the schedule's k-th cycle (k from 0, the anchor's).
func (s Schedule) slot(k int) Slot {
	start := s.Anchor.AddDate(0, 0, k*s.periodDays())
	return Slot{StartsAt: start, EndsAt: start.AddDate(0, 0, 7*s.Weeks)}
}

// firstFrom is the first slot starting at or after t (never before the anchor's).
func (s Schedule) firstFrom(t time.Time) int {
	if !t.After(s.Anchor) {
		return 0
	}
	k := int(t.Sub(s.Anchor) / (time.Duration(s.periodDays()) * 24 * time.Hour))
	for k > 0 && !s.slot(k-1).StartsAt.Before(t) {
		k--
	}
	for s.slot(k).StartsAt.Before(t) {
		k++
	}
	return k
}

// PlanSchedule is what the schedule does to a project's cycles at now. Pure.
//
// The next cycle starts on the first slot after the project's last cycle ends
// (one made by hand off the cadence included, so nothing overlaps); after a
// long pause the slots already over are skipped, so the cycle running now is
// made, not a run of past ones.
func PlanSchedule(s Schedule, cycles []cycleModel.Cycle, now time.Time) SchedulePlan {
	var plan SchedulePlan
	if s.Weeks < 1 {
		return plan
	}
	upcoming := min(max(s.Upcoming, 1), MaxUpcoming)
	var lastEnd time.Time
	future := 0
	ended := make([]cycleModel.Cycle, 0, 1)
	for _, c := range cycles {
		if c.EndsAt.After(lastEnd) {
			lastEnd = c.EndsAt
		}
		if c.CompletedAt != nil {
			continue
		}
		switch {
		case !c.EndsAt.After(now):
			ended = append(ended, c)
		case c.StartsAt.After(now):
			future++
		}
	}
	sort.Slice(ended, func(i, j int) bool { return ended[i].Number < ended[j].Number })
	for _, c := range ended {
		plan.Complete = append(plan.Complete, c.Id)
	}
	k := s.firstFrom(lastEnd)
	for !s.slot(k).EndsAt.After(now) {
		k++
	}
	for ; ; k++ {
		sl := s.slot(k)
		if sl.StartsAt.After(now) {
			if future >= upcoming {
				break
			}
			future++
		}
		plan.Create = append(plan.Create, sl)
	}
	return plan
}

// ScheduleInput is a schedule as a project's admin sets it.
type ScheduleInput struct {
	Weeks         int       `json:"weeks"`
	CooldownWeeks int       `json:"cooldown_weeks"`
	Upcoming      int       `json:"upcoming"`
	RollOver      bool      `json:"roll_over"`
	StartsAt      time.Time `json:"starts_at"`
	// AddStarted: a task that starts joins the cycle running then (startedJoins.go).
	AddStarted bool `json:"add_started"`
}

// Schedule checks the input, with words for the person. Pure.
func (in ScheduleInput) Schedule() (Schedule, error) {
	switch {
	case in.Weeks < 1 || in.Weeks > 8:
		return Schedule{}, &CycleError{"Cycles run 1 to 8 weeks."}
	case in.CooldownWeeks < 0 || in.CooldownWeeks > 1:
		return Schedule{}, &CycleError{"A cooldown is none or one week."}
	case in.Upcoming < 1 || in.Upcoming > MaxUpcoming:
		return Schedule{}, &CycleError{"Keep 1 to 15 cycles ready."}
	case in.StartsAt.IsZero():
		return Schedule{}, &CycleError{"Pick when the first cycle starts."}
	}
	return Schedule{Weeks: in.Weeks, CooldownWeeks: in.CooldownWeeks, Upcoming: in.Upcoming, RollOver: in.RollOver, Anchor: in.StartsAt}, nil
}
