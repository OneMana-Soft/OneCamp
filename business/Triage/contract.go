// Package business (Triage) is the Triage Queue
// (design/projects-vs-asana-jira/PROGRAM.md §4.2 #91 and D-PJ-5; B1 builds
// it): a project's incoming work (from forms, email, chat, the API and
// imports) waits in triage until someone accepts it, marks it a duplicate,
// declines, snoozes or moves it, with a reason, and one person a week may hold
// triage by rotation.
//
// DAY 0. No project has triage (projects.triage is off), so Decide answers
// ErrNotBuilt and Waiting answers nothing. Nothing calls Queue yet.
package business

import (
	"context"
	"errors"

	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Decision is what triage does with a task.
type Decision string

const (
	Accept    Decision = "accept"
	Duplicate Decision = "duplicate"
	Decline   Decision = "decline"
	Snooze    Decision = "snooze"
	Move      Decision = "move"
)

// Queue is a project's triage.
type Queue interface {
	// Waiting is the tasks waiting in a project's triage, oldest first.
	Waiting(ctx context.Context, who *userModels.UserInfo, projectID uuid.UUID) ([]string, error)
	// Decide decides a waiting task, with a reason.
	Decide(ctx context.Context, who *userModels.UserInfo, taskID uuid.UUID, d Decision, reason string) error
}

// ErrNotBuilt is triage before B1.
var ErrNotBuilt = errors.New("triage: that isn't built yet")

// Triage is the queue everything asks.
var Triage Queue = noTriage{}

type noTriage struct{}

func (noTriage) Waiting(context.Context, *userModels.UserInfo, uuid.UUID) ([]string, error) {
	return nil, nil
}

func (noTriage) Decide(context.Context, *userModels.UserInfo, uuid.UUID, Decision, string) error {
	return ErrNotBuilt
}
