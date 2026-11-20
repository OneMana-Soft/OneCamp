package business

// A task's timer, worked out from the task as it is now (Next), never from
// what a change changed: the WorkFeed sink calls it for every change to a
// task, as often as a change is delivered, and the same task gives the same
// timer. Pure.

import (
	"slices"
	"strings"
	"time"

	weekHours "github.com/akashc777/OneCamp/business/WeekHours"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/SLA"
	"github.com/google/uuid"
)

// Rule is one of a project's rules.
type Rule = model.Rule

// The clocks a rule counts its target on, and when a timer starts.
const (
	ClockBusiness = "business"
	ClockCalendar = "calendar"
	StartsArrival = "arrival"
)

// The places a task arrives from, as a rule names them.
var Sources = []string{"app", "api", "agent", "form"}

// The defaults' ids: fixed, so the app can name one; a timer under one names
// no rule (it has no row).
var (
	defaultUrgent = uuid.MustParse("6f2b1d0e-5a43-4c8e-9b7a-2f0c1d9e8a01")
	defaultHigh   = uuid.MustParse("6f2b1d0e-5a43-4c8e-9b7a-2f0c1d9e8a02")
)

// Defaults are the rules of a project that never saved one (linear-04 item
// 3, as Linear's): Urgent within a day, High within a week (five business
// days), both on the business clock.
func Defaults(project uuid.UUID) []Rule {
	return []Rule{
		{ID: defaultUrgent, ProjectID: project, Name: "Urgent", Priorities: []string{dgraphStruct.TASK_PRIORITY_URGENT},
			TargetMinutes: 24 * 60, Clock: ClockBusiness, Starts: StartsArrival, Rank: 0, Virtual: true},
		{ID: defaultHigh, ProjectID: project, Name: "High", Priorities: []string{dgraphStruct.TASK_PRIORITY_HIGH},
			TargetMinutes: 5 * 24 * 60, Clock: ClockBusiness, Starts: StartsArrival, Rank: 1, Virtual: true},
	}
}

// Facts are what a rule matches a task by: its priority, its labels, its type
// (TypeTask for a plain task) and where it arrived from.
type Facts struct {
	Priority string
	Labels   []string
	Type     uuid.UUID
	Source   string
}

// Matches is whether the rule takes a task with these facts: each list it
// sets holds the task's value (a label any of the task's), and an empty one
// takes anything.
func Matches(r Rule, f Facts) bool {
	if len(r.Priorities) > 0 && !slices.Contains(r.Priorities, f.Priority) {
		return false
	}
	if len(r.TypeIDs) > 0 && !slices.Contains(r.TypeIDs, f.Type) {
		return false
	}
	if len(r.Sources) > 0 && !slices.Contains(r.Sources, f.Source) {
		return false
	}
	if len(r.Labels) > 0 && !slices.ContainsFunc(f.Labels, func(l string) bool {
		return slices.ContainsFunc(r.Labels, func(want string) bool { return strings.EqualFold(want, l) })
	}) {
		return false
	}
	return true
}

// FirstMatch is the first of the rules, in rank order, that takes the task.
func FirstMatch(rules []Rule, f Facts) (Rule, bool) {
	for _, r := range rules {
		if Matches(r, f) {
			return r, true
		}
	}
	return Rule{}, false
}

// Task is a task as its timer reads it.
type Task struct {
	ID, Project uuid.UUID
	Facts
	// Status is its status's category; DoneAt when it became done.
	Status  string
	DoneAt  *time.Time
	Deleted bool
	// Arrived is when it was made.
	Arrived time.Time
}

// Why a timer ended.
const (
	EndedUnmatched = "unmatched"
	EndedCancelled = "cancelled"
	EndedDeleted   = "deleted"
	EndedDone      = "done"
)

// Next is the task's timer now, from the one it had (nil for none), the
// project's rules and work week: false keeps nothing. A task that arrives
// (arrived) dates its timer from its making; one first matched later, from
// that change (now). Once started, a timer keeps its start whatever rule
// takes the task next: the promise dates from the ask. Done before due is
// met, after it missed; cancelled, deleted or matched by no rule ends it,
// kept, so matching again resumes it.
func Next(prev *model.Timer, t Task, rules []Rule, hours weekHours.Hours, arrived bool, now time.Time) (model.Timer, bool) {
	rule, matched := FirstMatch(rules, t.Facts)
	next := model.Timer{TaskID: t.ID, ProjectID: t.Project, Source: t.Source, StartedAt: now}
	switch {
	case prev != nil:
		next.StartedAt, next.Source = prev.StartedAt, prev.Source
	case arrived:
		next.StartedAt = t.Arrived
	case !matched:
		return model.Timer{}, false
	}
	if !matched {
		return ended(next, prev, EndedUnmatched, now), true
	}
	if !rule.Virtual {
		id := rule.ID
		next.RuleID = &id
	}
	due, risk := DueOf(rule, next.StartedAt, hours)
	next.DueAt, next.RiskAt = &due, &risk
	switch {
	case t.Deleted:
		return ended(next, prev, EndedDeleted, now), true
	case t.Status == dgraphStruct.TASK_STATUS_CANCELED:
		return ended(next, prev, EndedCancelled, now), true
	case t.Status == dgraphStruct.TASK_STATUS_DONE:
		met := now
		if prev != nil && prev.MetAt != nil {
			met = *prev.MetAt
		} else if t.DoneAt != nil {
			met = *t.DoneAt
		}
		next.MetAt, next.EndedAt, next.EndedReason, next.State = &met, &met, EndedDone, model.Met
		if met.After(due) {
			next.State = model.Missed
		}
		return next, true
	}
	next.State = model.Running
	return next, true
}

// ended is t ended for why at now, or when prev ended for the same reason.
func ended(t model.Timer, prev *model.Timer, why string, now time.Time) model.Timer {
	if prev != nil && prev.State == model.Ended && prev.EndedReason == why && prev.EndedAt != nil {
		now = *prev.EndedAt
	}
	t.State, t.EndedAt, t.EndedReason, t.MetAt = model.Ended, &now, why, nil
	return t
}

// riskLead is how long before due a timer is at risk: a day of its clock, or
// the last quarter of a shorter target (a day's target isn't at risk from the
// start).
const riskLead = 24 * time.Hour

// DueOf is when a timer under rule started at start is due, and from when it
// is at risk, on the rule's clock: the business clock counts only the
// project's open hours, across closed days and daylight-saving changes.
func DueOf(rule Rule, start time.Time, hours weekHours.Hours) (due, risk time.Time) {
	target := time.Duration(rule.TargetMinutes) * time.Minute
	lead := min(riskLead, target/4)
	if rule.Clock == ClockCalendar {
		return start.Add(target), start.Add(target - lead)
	}
	due, ok := weekHours.After(hours, start, target)
	if !ok {
		due, ok = weekHours.After(DefaultHours(hours.TZ), start, target)
	}
	if !ok {
		return start.Add(target), start.Add(target - lead)
	}
	risk, _ = weekHours.After(hours, start, target-lead)
	if risk.IsZero() {
		risk, _ = weekHours.After(DefaultHours(hours.TZ), start, target-lead)
	}
	return due, risk
}

// DefaultHours is a project's work week until its admins set one: Monday to
// Friday, all day, in zone (UTC for none or one this server can't read).
func DefaultHours(zone string) weekHours.Hours {
	if _, err := time.LoadLocation(zone); err != nil || zone == "" {
		zone = "UTC"
	}
	return weekHours.Hours{Days: []int{1, 2, 3, 4, 5}, Start: "00:00", End: "24:00", TZ: zone}
}

// StateAt is where a kept timer stands at now.
func StateAt(t model.Timer, now time.Time) State {
	switch t.State {
	case model.Met:
		return Met
	case model.Missed:
		return Breached
	case model.Running:
	default:
		return None
	}
	switch {
	case t.DueAt == nil:
		return None
	case !now.Before(*t.DueAt):
		return Breached
	case t.RiskAt != nil && !now.Before(*t.RiskAt):
		return AtRisk
	}
	return OnTrack
}

// typeOf is the type a rule matches a task by: a plain task's is TypeTask.
func typeOf(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return workTaxonomy.TypeTask
	}
	return *id
}
