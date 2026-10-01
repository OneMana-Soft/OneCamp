package business

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// MoveFilter narrows "a task changed status" to the moves someone cares about:
// in one project, into one status. Workflows and agents both take one, so
// "when a task moves to QA" means the same thing to either. Both fields are
// optional; empty matches everything.
type MoveFilter struct {
	ProjectID string `json:"project_id,omitempty"`
	// ToStatus is a built-in key or a custom status's id (never a name, which
	// a rename would break). A built-in key matches a task whose category
	// becomes it, so "done" covers a project's own statuses counting as Done;
	// an id matches a task entering that status.
	ToStatus string `json:"to_status,omitempty"`
}

// Move is one task's status change, as the task.status_changed event says it.
type Move struct {
	ProjectID            string
	OldStatus, NewStatus string // categories
	OldCustom, NewCustom string // custom status ids, "" for none
}

// MoveFromEvent reads a task.status_changed event's payload.
func MoveFromEvent(data map[string]interface{}) Move {
	s := func(k string) string { v, _ := data[k].(string); return v }
	return Move{
		ProjectID: s("project_id"),
		OldStatus: s("old_status"), NewStatus: s("new_status"),
		OldCustom: s("old_custom_id"), NewCustom: s("new_custom_id"),
	}
}

// Matches reports whether the filter wants this move.
func (f MoveFilter) Matches(m Move) bool {
	if f.ProjectID != "" && f.ProjectID != m.ProjectID {
		return false
	}
	switch to := f.ToStatus; {
	case to == "":
		return true
	case IsBuiltIn(to):
		return m.NewStatus == to && m.OldStatus != to
	default:
		return m.NewCustom == to && m.OldCustom != to
	}
}

// NormalizeMoveFilter checks a filter as a person set it (a status by name,
// "QA" or "In review") and returns it as it is stored and compared. A
// project's own status needs its project; a name the project does not have is
// an error that lists the ones it does.
func NormalizeMoveFilter(ctx context.Context, projectID, toStatus string) (MoveFilter, error) {
	f := MoveFilter{ProjectID: strings.TrimSpace(projectID)}
	if f.ProjectID != "" {
		if _, err := uuid.Parse(f.ProjectID); err != nil {
			return MoveFilter{}, fmt.Errorf("that is not a project")
		}
	}
	status := strings.TrimSpace(toStatus)
	if status == "" {
		return f, nil
	}
	r, err := Resolve(ctx, f.ProjectID, status)
	if errors.Is(err, ErrUnknownStatus) {
		if f.ProjectID == "" {
			return MoveFilter{}, fmt.Errorf("%q is not a status every project has; pick the project it belongs to", status)
		}
		return MoveFilter{}, fmt.Errorf("%q is not a status of that project. %s", status, Describe(ctx, f.ProjectID))
	}
	if err != nil {
		return MoveFilter{}, err
	}
	f.ToStatus = r.Value()
	return f, nil
}
