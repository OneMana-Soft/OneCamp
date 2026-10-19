// Package business (SLA) is SLA Timers
// (design/projects-vs-asana-jira/PROGRAM.md §4.2 #92 and D-PJ-15; B2 builds
// it): rules by priority, type, label and project set a response or
// resolution target in a project's business hours, and a claimed sweep moves
// each task's timer between on track, at risk, breached and met.
//
// DAY 0. No rule exists, so every task is on no timer: StateOf answers None.
// Nothing calls Timers yet.
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
var SLAs Timers = noTimers{}

type noTimers struct{}

func (noTimers) StateOf(context.Context, uuid.UUID) (State, *time.Time, error) {
	return None, nil, nil
}
