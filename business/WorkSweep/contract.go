// Package business (WorkSweep) is the projects programme's sweeps
// (design/projects-vs-asana-jira/PROGRAM.md §5), registered with the Sweeper
// (#14, claimed so one instance runs each pass) once, at day 0, under names
// that are never renamed. Each runs what its package sets from its own init:
//
//	func init() { workSweep.Cycles = runCycles }
//
// so no package registers a second job by the same name, and main.go has no
// line for them.
//
// DAY 0. Nothing is set, so every pass does nothing.
package business

import (
	"context"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
)

// What each sweep runs; a package sets its own from its init.
var (
	// Cycles starts, ends and rolls over scheduled cycles (B3).
	Cycles = nothing
	// SLA moves tasks' SLA timers (B2).
	SLA = nothing
	// Reminders sends project update reminders (B5).
	Reminders = nothing
	// AutoClose closes tasks nobody touched for a project's
	// projects.autoclose.days.
	AutoClose = nothing
	// InsightsRollup rolls the day's work metrics up (B9).
	InsightsRollup = nothing
)

func nothing(context.Context) error { return nil }

// The sweeps' names and intervals, never renamed.
var jobs = []struct {
	name  string
	every time.Duration
	run   *func(context.Context) error
}{
	{"projects.cycles", 15 * time.Minute, &Cycles},
	{"projects.sla", time.Minute, &SLA},
	{"projects.reminders", 15 * time.Minute, &Reminders},
	{"projects.autoclose", time.Hour, &AutoClose},
	{"projects.insights.rollup", time.Hour, &InsightsRollup},
}

func init() {
	for _, j := range jobs {
		run := j.run
		sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: j.name, Every: j.every, Run: func(ctx context.Context) error { return (*run)(ctx) }})
	}
}
