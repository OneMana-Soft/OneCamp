package business

import (
	"context"
	"strings"

	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Who a task may be given to. An assignee reads their task, in its project or
// not and restricted or not (domain/Task TaskReader.MayRead, Readers), so
// whoever a task was given to could read it: someone with the right to edit a
// task could hand it to a person outside its project, or past a restriction an
// admin set. So a task goes only to someone who may read it already, and
// anyone else is refused in words, before anything is written.
//
// Three things keep their ways. A task in no project is its maker's own, with
// no admin's boundary: it goes to any active member of the workspace, not to an
// identity that can't sign in. A bot is given work by assignment (an AI
// teammate picks up what it's given), as before. And a data move (DataMove: an
// import, a template's apply, the demo's seed, GitHub's sync), made by someone
// with the right to make it, brings its people with it.

// Seams for tests.
var (
	seeingOf     = domain.SeeingOf
	membersAmong = projectDomain.MembersAmong
)

// Unseeing is the refusal (a conflict, in words) for giving the task (taskID,
// as task has it now) to to: nil when to may read it already, is its assignee
// already, or is one of the ways above.
func Unseeing(ctx context.Context, taskID string, task *dgraphStruct.DgraphTask, to *dgraphStruct.DgraphUser) error {
	if to == nil || keepsItsWay(ctx, to) {
		return nil
	}
	if task != nil && task.Assignee != nil && task.Assignee.Uuid != "" && strings.EqualFold(task.Assignee.Uuid, to.Uuid) {
		return nil
	}
	s, err := seeingOf(ctx, taskID, to.Uuid)
	if err != nil {
		return err
	}
	return refusalFor(s, to)
}

// UnseeingNew is Unseeing for a task about to be made in the project: its
// members and admins see it. A sub-task (parent, its parent's id) is seen by
// whoever sees its parent, which it is restricted with.
func UnseeingNew(ctx context.Context, projectID, parent string, to *dgraphStruct.DgraphUser) error {
	if to == nil || keepsItsWay(ctx, to) {
		return nil
	}
	if parent != "" {
		s, err := seeingOf(ctx, parent, to.Uuid)
		if err != nil {
			return err
		}
		return refusalFor(s, to)
	}
	if projectID == "" {
		return outsideProjects(to)
	}
	in, err := membersAmong(ctx, projectID, []string{to.Uuid})
	if err != nil {
		return err
	}
	return refusalFor(domain.Seeing{Sees: in[to.Uuid], InProject: true}, to)
}

// keepsItsWay is a bot, or a data move.
func keepsItsWay(ctx context.Context, to *dgraphStruct.DgraphUser) bool {
	return to.IsBot || ctx.Value(dataMoveKey{}) != nil
}

// refusalFor is what giving a task to to answers, as they stand to it (s).
func refusalFor(s domain.Seeing, to *dgraphStruct.DgraphUser) error {
	switch {
	case !s.InProject:
		return outsideProjects(to)
	case s.Sees:
		return nil
	case s.Restricted:
		return helpers.Conflict(nameOf(to)+" can't see this task: only some people can see it. A project admin can let them in.", nil)
	}
	return helpers.Conflict(nameOf(to)+" can't see this task. Add them to the project first.", nil)
}

// outsideProjects is a task in no project: an active member of the workspace
// may be given it (a deactivated one is refused by Unassignable); an identity
// that can't sign in (an imported author, an unmapped GitHub account) isn't one.
func outsideProjects(to *dgraphStruct.DgraphUser) error {
	if to.IsExternal {
		return helpers.Conflict(nameOf(to)+" can't be given a task outside a project: they aren't a member of this workspace.", nil)
	}
	return nil
}

func nameOf(u *dgraphStruct.DgraphUser) string {
	if who := u.DisplayName(); who != "" {
		return who
	}
	return "That person"
}
