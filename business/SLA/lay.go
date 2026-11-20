package business

// Where a task's timer stands rides on the task reads members get, laid as
// keys and types are (business/TaskKeys Lay): one read of the timers for a
// whole answer, then, only when some have one, one read of which of their
// projects the reader is in. Only a project's members (its viewers to its
// admins) read its SLA state: anyone else gets none, a guest none even with a
// member's place (one read of the reader, only when they're in one), and
// neither does a project whose SLAs are off.

import (
	"context"
	"errors"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/pgexec"
	model "github.com/akashc777/OneCamp/models/postgres/SLA"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// Seams for tests: the timers, who is in which projects, and who reads.
var (
	timersRead   = model.TimersRead
	membersAmong = projectDomain.MembersAmongEach
	readerOf     = taskread.ReaderOf
	isGuest      = userDomain.ReadsAsGuest
	onIn         = func(ctx context.Context, projects []string) (map[string]bool, error) {
		return policyBusiness.Admin.BoolsIn(ctx, policyBusiness.ProjectsSLA, policyBusiness.ScopeProject, projects)
	}
)

// layTimers sets each task's SLA state for its reader. It fails closed: a
// task leaves with none unless all is read and the reader may read it, and a
// task they see only as restricted (a stub, models/taskread) never carries
// any, nor is its timer read, whoever hands it here (V-1057). The timers'
// read goes in the batch of the Lay it is part of (TaskKeys.Planned); who
// reads which projects waits on it, so it is read after (layRead).
func layTimers(ctx context.Context, tasks []*dgraphStruct.DgraphTask) pgexec.Planned {
	shown := make([]*dgraphStruct.DgraphTask, 0, len(tasks))
	ids := make([]uuid.UUID, 0, len(tasks))
	for _, t := range tasks {
		t.SLA = nil
		if t.Hidden {
			continue
		}
		if id, err := uuid.Parse(t.Uuid); err == nil {
			shown = append(shown, t)
			ids = append(ids, id)
		}
	}
	reader := readerOf(ctx)
	if _, err := uuid.Parse(reader); err != nil || len(ids) == 0 {
		return pgexec.Planned{}
	}
	read, timersOf := timersRead(ids)
	return pgexec.Planned{Reads: []*pgexec.Read{read}, Then: func() { layRead(ctx, reader, shown, ids, timersOf) }}
}

// layRead lays the timers read for the tasks shown (ids, in the same order)
// once the batch they were read in has run.
func layRead(ctx context.Context, reader string, shown []*dgraphStruct.DgraphTask, ids []uuid.UUID, timersOf func() (map[uuid.UUID]model.Timer, error)) {
	timers, err := timersOf()
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/SLA couldn't read %d tasks' timers: %+v", len(ids), err)
		return
	}
	if len(timers) == 0 {
		return
	}
	seen := map[string]bool{}
	var projects []string
	for _, t := range timers {
		if p := t.ProjectID.String(); !seen[p] {
			seen[p] = true
			projects = append(projects, p)
		}
	}
	reads, err := readsIn(ctx, reader, projects)
	if errors.Is(err, errGuest) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/SLA couldn't read who reads %d projects' SLA state; none laid: %+v", len(projects), err)
		return
	}
	now := clock()
	for i, t := range shown {
		timer, ok := timers[ids[i]]
		if !ok || !reads[timer.ProjectID.String()] {
			continue
		}
		if s := StateAt(timer, now); s != None {
			t.SLA = &dgraphStruct.TaskSLA{State: string(s), DueAt: timer.DueAt, RiskAt: timer.RiskAt}
		}
	}
}
