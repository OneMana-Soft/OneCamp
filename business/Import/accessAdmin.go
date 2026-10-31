package business

// What the admins' card shows of old imports' access re-check (recheck.go),
// and keeping one as it is, the admin's fourth choice beside re-checking it
// with their own connection, undoing it and containing it.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
)

// ImportAccess is one old import on the admins' card: its tool and site, when
// it first ran and who ran it, where it stands (its least settled project's
// state), why it needs an admin, who decided last, and its projects.
type ImportAccess struct {
	ImportID  uuid.UUID             `json:"import_id"`
	Provider  string                `json:"provider"`
	Site      string                `json:"site"`
	RanAt     time.Time             `json:"ran_at"`
	RanBy     string                `json:"ran_by"`
	State     string                `json:"state"`
	Reason    string                `json:"reason,omitempty"`
	DecidedBy string                `json:"decided_by,omitempty"`
	DecidedAt *time.Time            `json:"decided_at,omitempty"`
	Projects  []ImportAccessProject `json:"projects"`
}

// ImportAccessProject is one of an import's projects, archived ones too
// (ruling G), with where it stands and its reviews' counts together.
type ImportAccessProject struct {
	ProjectUUID uuid.UUID `json:"project_uuid"`
	Name        string    `json:"name"`
	Archived    bool      `json:"archived"`
	State       string    `json:"state"`
	AccessCounts
}

// AccessCounts is what leaves the server of a review's summary: its counts,
// and the tasks it names by id (never a title), each opened through the task's
// own route as the admin. A pass's cursor, tries and who re-checked stay here,
// and the tasks a pass failed on are named only once they keep failing
// (reasonTasksFailing).
type AccessCounts struct {
	Restricted           int         `json:"restricted"`
	Unassigned           int         `json:"unassigned"`
	AlreadyRestricted    int         `json:"already_restricted"`
	OpenedSince          int         `json:"opened_since"`
	Opened               int         `json:"opened"`
	CreatorOutside       int         `json:"creator_outside"`
	CreatorOutsideTasks  []uuid.UUID `json:"creator_outside_tasks,omitempty"`
	AssigneeOutside      int         `json:"assignee_outside"`
	AssigneeOutsideTasks []uuid.UUID `json:"assignee_outside_tasks,omitempty"`
	CommentsHidden       int         `json:"comments_hidden"`
	GoneOrHidden         int         `json:"gone_or_hidden"`
	GoneOrHiddenTasks    []uuid.UUID `json:"gone_or_hidden_tasks,omitempty"`
	Failed               []uuid.UUID `json:"failed,omitempty"`
}

// add counts in a review's summary.
func (a *AccessCounts) add(c recheckCounts) {
	a.Restricted += c.Restricted
	a.Unassigned += c.Unassigned
	a.AlreadyRestricted += c.AlreadyRestricted
	a.OpenedSince += c.OpenedSince
	a.Opened += c.Opened
	a.CreatorOutside += c.CreatorOutside
	a.CreatorOutsideTasks = append(a.CreatorOutsideTasks, c.CreatorOutsideTasks...)
	a.AssigneeOutside += c.AssigneeOutside
	a.AssigneeOutsideTasks = append(a.AssigneeOutsideTasks, c.AssigneeOutsideTasks...)
	a.CommentsHidden += c.CommentsHidden
	a.GoneOrHidden += c.GoneOrHidden
	a.GoneOrHiddenTasks = append(a.GoneOrHiddenTasks, c.GoneOrHiddenTasks...)
	if c.Reason == reasonTasksFailing {
		a.Failed = append(a.Failed, c.Failed...)
	}
}

// unsettled orders the states from the least settled: an import or project
// stands where its least settled review does.
var unsettled = map[string]int{
	reviewModel.StateRechecking: 0,
	reviewModel.StateNeedsAdmin: 1,
	reviewModel.StateContained:  2,
	reviewModel.StateRechecked:  3,
	reviewModel.StateKept:       4,
	reviewModel.StateUndone:     5,
}

// ImportAccessList reads the admins' card: each old import whose access is
// re-checked, the newest first.
func ImportAccessList(ctx context.Context) ([]ImportAccess, error) {
	rows, err := reviewModel.AccessImportRows(ctx)
	if err != nil {
		return nil, err
	}
	return importsOf(rows), nil
}

// importsOf groups the reviews, in the order read, by import and project.
func importsOf(rows []reviewModel.AccessImportRow) []ImportAccess {
	out := []ImportAccess{}
	for i := range rows {
		r := &rows[i]
		if len(out) == 0 || out[len(out)-1].ImportID != r.ImportID {
			out = append(out, ImportAccess{ImportID: r.ImportID, Provider: r.Provider, Site: r.Site, RanAt: r.RanAt,
				RanBy: r.RanBy, State: r.State, Projects: []ImportAccessProject{}})
		}
		imp := &out[len(out)-1]
		ps := imp.Projects
		if len(ps) == 0 || ps[len(ps)-1].ProjectUUID != r.ProjectUUID {
			imp.Projects = append(ps, ImportAccessProject{ProjectUUID: r.ProjectUUID, Name: r.ProjectName,
				Archived: r.Archived, State: r.State})
		}
		p := &imp.Projects[len(imp.Projects)-1]
		c := countsOf(&r.AccessReview)
		p.add(c)
		if unsettled[r.State] < unsettled[p.State] {
			p.State = r.State
		}
		if unsettled[r.State] < unsettled[imp.State] {
			imp.State = r.State
		}
		if r.State == reviewModel.StateNeedsAdmin && c.Reason != "" {
			imp.Reason = c.Reason
		}
		if r.DecidedAt != nil && (imp.DecidedAt == nil || r.DecidedAt.After(*imp.DecidedAt)) {
			imp.DecidedAt, imp.DecidedBy = r.DecidedAt, r.DecidedByName
		}
	}
	return out
}

// KeepImportAccess keeps, as the admin by, an import whose reviews wait for
// an admin as it is: nothing it brought is narrowed, and its held-back
// comments go into search at once. Only from waiting
// for an admin: a contained import is re-checked or undone instead, since a
// kept one's tasks would stay with their projects' admins with nothing left
// to undo. It moves under the import's lock, so it can't land while a
// containment or re-check of the import runs.
func KeepImportAccess(ctx context.Context, importID, by uuid.UUID) error {
	if _, err := jobAwaitingAdmin(ctx, importID, reviewModel.StateNeedsAdmin); err != nil {
		return err
	}
	unlock, err := lockImport(ctx, importID)
	if errors.Is(err, errRecheckWaits) {
		return ErrRecheckRunning
	}
	if err != nil {
		return err
	}
	defer unlock()
	moved, err := reviewModel.MoveAccessReviews(ctx, importID, []string{reviewModel.StateNeedsAdmin},
		reviewModel.StateKept, nil, &by)
	if err != nil {
		return err
	}
	if moved == 0 {
		return ErrNotRecheckable
	}
	releaseInBackground(ctx)
	return nil
}
