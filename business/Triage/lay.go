package business

import (
	"context"
	"strings"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// Lay sets project_triage on a project read for reader (a user's uuid) of
// standing s, the standing the read already holds, so nothing is read again:
// on, and how many wait that no snooze holds back now, of those the reader's
// own queue lists, so the app's Triage tab needs no read of its own to show.
// Only a reader the queue's read admits gets it (NeedView in the project, as
// mayIn asks); one it refuses, a project with triage off, or a count that
// fails (logged) gets none, which the app reads as off.
//
// The count is the reader's: one the rule lets into every restricted task of
// the project (its admins) counts every waiting task; anyone else leaves out
// the restricted ones whose rows keep them out, as their queue does (a token
// counts as the member it acts as). A reader who isn't a person is kept out
// of every restricted task, as taskread keeps nobody out.
//
// Store calls: none for a reader the queue refuses; when off, the
// projects.triage policy alone, from the project's values the standing's own
// read of its policies keeps (projects.edit.who); when on, one count.
func Lay(ctx context.Context, reader string, s workAccess.Standing, p *dgraphStruct.DgraphProject) {
	if p == nil || !s.Can(workAccess.NeedView) || !strings.EqualFold(s.ProjectUUID, p.Uuid) {
		return
	}
	id, err := uuid.Parse(p.Uuid)
	if err != nil || !triageOn(ctx, p.Uuid) {
		return
	}
	var n int
	if s.LetIn(workAccess.TaskFacts{Restricted: true}) {
		n, err = countAwake(ctx, id)
	} else {
		me, _ := uuid.Parse(strings.TrimSpace(reader)) // uuid.Nil for one who isn't a person
		n, err = countAwakeFor(ctx, id, me)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Triage: counting what waits in project %s's triage: %v", id, err)
		return
	}
	p.Triage = &dgraphStruct.ProjectTriage{On: true, Waiting: n}
}
