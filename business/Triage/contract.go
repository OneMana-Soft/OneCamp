// Package business (Triage) is the Triage Queue
// (design/projects-vs-asana-jira/PROGRAM.md §4.2 #91 and D-PJ-5; package 9
// R5a): a project with triage on (its projects.triage policy) takes the work
// that comes in from outside its board (a form, the API, an agent, GitHub, a
// chat, an import) into triage until one of its people accepts it, marks it
// a duplicate, declines it or snoozes it, with a reason. What waits leaves the project's lists and counts
// (models/taskread Waiting) and is hidden from no one: the task itself, a
// search and the queue still read it. Triage is a mark beside the status
// (models/postgres/Triage), never a status, and the task node in the graph
// is unchanged.
package business

import (
	"context"
	"errors"
	"time"

	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Decision is what triage does with a task.
type Decision string

const (
	// Accept makes it the team's work, in the status chosen, or as it is.
	Accept Decision = "accept"
	// Duplicate cancels it, and says on the original that it was asked again.
	Duplicate Decision = "duplicate"
	// Decline cancels it, saying why on the task.
	Decline Decision = "decline"
	// Snooze keeps it waiting, out of the queue until a time.
	Snooze Decision = "snooze"
)

// Choice is a decision and what it takes: a reason (for any; a decline's is
// said on the task), the status an accepted task starts in (optional), the
// original of a duplicate, and when a snoozed task comes back.
type Choice struct {
	Decision    Decision  `json:"decision"`
	Reason      string    `json:"reason"`
	Status      string    `json:"status"`
	DuplicateOf uuid.UUID `json:"duplicate_of"`
	Until       time.Time `json:"until"`
}

// Queue is a project's triage.
type Queue interface {
	// Waiting is what waits in a project's triage that the person may read,
	// oldest first, snoozed or not; none while the project has triage off.
	Waiting(ctx context.Context, who *userModels.UserInfo, projectID uuid.UUID) ([]Request, error)
	// Decide decides a task waiting in its project's triage.
	Decide(ctx context.Context, who *userModels.UserInfo, taskID uuid.UUID, c Choice) error
}

// ErrNotWaiting is a decision on a task that isn't waiting in triage: decided
// already, or never in it.
var ErrNotWaiting = errors.New("triage: that task isn't waiting in triage")

// ErrTriageOff is a decision in a project with triage off.
var ErrTriageOff = errors.New("triage: this project has triage off")

// ErrBadChoice is a decision that is missing what it takes, or names
// something it can't use.
var ErrBadChoice = errors.New("triage: that decision can't be made")

// ErrNeedsPriority is an accept in a project that asks for a priority before
// work leaves triage (projects.triage.priority), of a task without one.
var ErrNeedsPriority = errors.New("triage: give it a priority before accepting it")

// Triage is the queue everything asks.
var Triage Queue = queue{}
