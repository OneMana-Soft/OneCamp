package business

// The release field on every task (jira-04): whoever may change a task puts
// it in the releases of its project, or takes it out, as they set its cycle;
// a release already out changes only by the project's admins. The same rows
// as a release's own task list (AddTasks), so each shows the other at once.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
	"github.com/google/uuid"
)

// FilterID is the task list filter on releases (business/Project's side
// filters); NoRelease, among its values, is the tasks in none of them.
const (
	FilterID  = "task_release"
	NoRelease = "none"
)

// The refusals of a change to a task's releases, and the one of an id that
// isn't one of its project's releases.
const (
	archivedTask = "This project is archived, so its tasks' releases can't change. Restore the project first."
	outAdmins    = "Only the project's admins change the tasks of a release that's out."
	notOurs      = "That isn't one of the releases of this task's project."
)

// Seams for tests: the field's reads and its write.
var (
	taskStanding  = workAccess.OfTask
	releasesOf    = releaseModel.OfTask
	releaseRows   = releaseModel.GetMany
	setTaskRows   = releaseModel.SetTask
	tasksFiltered = releaseModel.TasksFiltered
)

// TaskReleases is a task's release field as its reader sees it.
type TaskReleases struct {
	// Releases are the live releases of its project it is in, newest first.
	Releases []releaseModel.Release `json:"releases"`
	// Options are those it could be in: its project's live releases, newest
	// made first, PageMost of them.
	Options []releaseModel.Release `json:"options"`
	// CanEdit is whether the reader may set it: they may change the task,
	// and its project isn't archived.
	CanEdit bool `json:"can_edit"`
	// CanChangeReleased is whether they may also put it in, or take it out
	// of, a release that's out: the project's admins.
	CanChangeReleased bool `json:"can_change_released"`
}

// Refused is a change the person may not make, in words (a 403).
type Refused struct{ msg string }

func (e *Refused) Error() string { return e.msg }

// FilterClause narrows a project's task list to the tasks in the given
// releases and, with NoRelease among them, the tasks in none of its
// releases: one read, whose size is bounded by the project's released work.
// The ids are the person's: one that isn't a live release of this project
// matches nothing.
func FilterClause(ctx context.Context, project uuid.UUID, values []string) (string, error) {
	var chosen []uuid.UUID
	none := false
	for _, v := range values {
		if v == NoRelease {
			none = true
		} else if id, err := uuid.Parse(v); err == nil {
			chosen = append(chosen, id)
		}
	}
	if !none && len(chosen) == 0 {
		return taskDomain.NoTask, nil
	}
	tasks, err := tasksFiltered(ctx, project, chosen, none)
	if err != nil {
		return "", err
	}
	return taskDomain.TasksClause(tasks, none), nil
}

// projectOf is task t's project, uuid.Nil when it is in none. Pure.
func projectOf(t *dgraphStruct.DgraphTask) uuid.UUID {
	if t == nil || t.Project == nil {
		return uuid.Nil
	}
	id, _ := uuid.Parse(t.Project.Uuid)
	return id
}

// ForTask is task t's release field as who sees it (t read for them
// already). Its releases are its project's: someone outside the project (an
// assignee from elsewhere) sees none of them.
func ForTask(ctx context.Context, who accessBusiness.Subject, t *dgraphStruct.DgraphTask) (TaskReleases, error) {
	out := TaskReleases{Releases: []releaseModel.Release{}, Options: []releaseModel.Release{}}
	project := projectOf(t)
	if project == uuid.Nil {
		return out, nil
	}
	s, f, err := taskStanding(ctx, who.UserUUID, who.UserUID, t)
	if err != nil || !s.Can(workAccess.NeedView) {
		return out, err
	}
	task, _ := uuid.Parse(t.Uuid)
	in, err := releasesOf(ctx, project, task)
	if err != nil {
		return out, err
	}
	options, err := listRows(ctx, project, uuid.Nil, PageMost)
	if err != nil {
		return out, err
	}
	out.Releases, out.Options = append(out.Releases, in...), append(out.Options, options...)
	out.CanEdit = !s.Archived && s.CanOnTask(workAccess.NeedEditTask, f)
	out.CanChangeReleased = out.CanEdit && s.Can(workAccess.NeedManage)
	return out, nil
}

// SetForTask sets the releases task t is in to raw (the request's release
// ids, the whole set), for who, who may change t (the route checked
// NeedEditTask): t joins those it isn't in, from source manual, and leaves,
// softly, those not named, in one transaction. The ids are the client's,
// never trusted: each must be a live release of t's own project, else
// nothing changes and the words say so. An archived project's tasks change
// none, and a release that's out changes only by the project's admins.
// Answers the field as it then stands.
func SetForTask(ctx context.Context, who accessBusiness.Subject, t *dgraphStruct.DgraphTask, raw []string) (TaskReleases, error) {
	none := TaskReleases{}
	project := projectOf(t)
	if project == uuid.Nil {
		return none, &ReleaseError{"This task isn't in a project, so it has no releases."}
	}
	if !inProject(t, project) {
		return none, helpers.ErrNotFound
	}
	if len(raw) > MostAtOnce {
		return none, &ReleaseError{fmt.Sprintf("Put a task in at most %d releases at a time.", MostAtOnce)}
	}
	by, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return none, helpers.ErrNotFound
	}
	s, _, err := taskStanding(ctx, who.UserUUID, who.UserUID, t)
	if err != nil {
		return none, err
	}
	if s.Archived {
		return none, &Refused{archivedTask}
	}
	var asked []uuid.UUID
	named := map[uuid.UUID]bool{}
	for _, v := range raw {
		id, err := uuid.Parse(strings.TrimSpace(v))
		if err != nil {
			return none, &ReleaseError{"That isn't a release."}
		}
		if !named[id] {
			named[id] = true
			asked = append(asked, id)
		}
	}
	rows, err := releaseRows(ctx, asked)
	if err != nil {
		return none, err
	}
	release := map[uuid.UUID]releaseModel.Release{}
	for _, r := range rows {
		release[r.ID] = r
	}
	for _, id := range asked {
		if r, ok := release[id]; !ok || r.ProjectID != project {
			return none, &ReleaseError{notOurs}
		}
	}
	task, _ := uuid.Parse(t.Uuid)
	current, err := releasesOf(ctx, project, task)
	if err != nil {
		return none, err
	}
	var add, remove []uuid.UUID
	var changed []releaseModel.Release
	in := map[uuid.UUID]bool{}
	for _, r := range current {
		in[r.ID] = true
		if !named[r.ID] {
			remove = append(remove, r.ID)
			changed = append(changed, r)
		}
	}
	for _, id := range asked {
		if !in[id] {
			add = append(add, id)
			changed = append(changed, release[id])
		}
	}
	for _, r := range changed {
		if r.State == releaseModel.Released && !s.Can(workAccess.NeedManage) {
			return none, &Refused{outAdmins}
		}
	}
	err = setTaskRows(ctx, task, add, remove, by, MostTasks)
	switch {
	case errors.Is(err, releaseModel.ErrFull):
		return none, &ReleaseError{fmt.Sprintf("A release holds at most %d tasks.", MostTasks)}
	case errors.Is(err, helpers.ErrNotFound):
		return none, &ReleaseError{notOurs}
	case err != nil:
		return none, err
	}
	return ForTask(ctx, who, t)
}
