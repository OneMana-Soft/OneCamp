// Package business (SLA) is SLA Timers
// (design/projects-vs-asana-jira/PROGRAM.md §4.2 #92 and D-PJ-15; B2 builds
// it): rules by priority, type, label and project set a response or
// resolution target in a project's business hours, and a claimed sweep moves
// each task's timer between on track, at risk, breached and met.
//
// linear-24 H1: a project's admins turn timers on (projects.sla) and set its
// rules and work week (settings.go); the Work Change Log's sink keeps each
// task's timer (sink.go, timer.go); task reads carry where it stands for the
// project's members (lay.go). Alerts and the sweep that sends them are H2.
package business

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// State is where a task's timer stands.
type State string

const (
	None     State = ""
	OnTrack  State = "on_track"
	AtRisk   State = "at_risk"
	Breached State = "breached"
	Met      State = "met"
)

// Timers says where tasks' timers stand.
type Timers interface {
	// StateOf is the task's timer's state and when it is due (nil for none).
	StateOf(ctx context.Context, taskID uuid.UUID) (State, *time.Time, error)
}

// SLAs is the timers everything asks.
var SLAs Timers = kept{}

type kept struct{}

// StateOf is where the task's kept timer stands now, None while its
// project's SLAs are off. It answers the project, not a reader: a caller
// showing it to someone checks they are one of the project's members.
func (kept) StateOf(ctx context.Context, taskID uuid.UUID) (State, *time.Time, error) {
	t, found, err := timerOf(ctx, taskID)
	if err != nil || !found || !slaOn(ctx, t.ProjectID) {
		return None, nil, err
	}
	s := StateAt(t, clock())
	if s == None {
		return None, nil, nil
	}
	return s, t.DueAt, nil
}
