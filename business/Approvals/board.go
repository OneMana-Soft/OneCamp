package business

import (
	"context"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
)

// MoveInto sets on a board's project access each status with a rule on who
// moves tasks there, and whether its reader may (whoMay, as the gate asks it
// of a move), so the board offers no New task where the server would refuse
// one. A custom status's own rule comes before its built-in category's, as
// in ruleFor. Its one read is the project's rules; a read that fails leaves
// it unset, and the server still refuses such a move in words.
func MoveInto(ctx context.Context, readerUUID string, p *dgraphStruct.DgraphProject) {
	if p == nil || p.Access == nil {
		return
	}
	projectID, err := uuid.Parse(p.Uuid)
	if err != nil {
		return
	}
	rules, err := listRules(ctx, projectID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Approvals: reading project %s's status rules for its board: %v", projectID, err)
		return
	}
	if len(rules) == 0 {
		return
	}
	manage := func() bool { return p.Access.Manage }
	p.Access.MoveInto = make(map[string]bool, len(rules))
	for _, r := range rules {
		p.Access.MoveInto[r.Status] = whoMay(r, readerUUID, manage)
	}
}

// MarkWaiting lays each task's open request for approval over a board's read
// (dgraph TaskWaiting), in one read for them all, so a held card says it's
// waiting without a fetch of its own. The tasks are the ones the reader was
// given, so it shows nothing more than each task's own page does. A read that
// fails leaves the cards unmarked.
func MarkWaiting(ctx context.Context, tasks []*dgraphStruct.DgraphTask) {
	ids := make([]uuid.UUID, 0, len(tasks))
	for _, t := range tasks {
		if t == nil {
			continue
		}
		if id, err := uuid.Parse(t.Uuid); err == nil {
			ids = append(ids, id)
		}
	}
	open, err := model.PendingForTasks(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Approvals: marking a board's held cards: %v", err)
		return
	}
	byTask := make(map[string]*model.Approval, len(open))
	for _, a := range open {
		byTask[a.TaskID.String()] = a
	}
	for _, t := range tasks {
		if t == nil {
			continue
		}
		if a := byTask[t.Uuid]; a != nil {
			t.Waiting = &dgraphStruct.TaskWaiting{ApprovalID: a.ID.String(), ToStatus: a.ToStatus}
		}
	}
}
