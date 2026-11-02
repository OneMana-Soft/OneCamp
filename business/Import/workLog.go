package business

// The Work Change Log (business/WorkFeed, WCL-1) from the importer's side: a
// task an import makes is logged as made, as one made in the app is, once a
// written batch (the task chunk's 25 at a time, or what a chunk wrote when it
// ends), so a board open on its project shows it as it comes and a reader
// catching up from the log has it. The funnel tells a batch's entries one by
// one, or a bigger one as one Reread a project.

import (
	"context"

	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// madeTasks is what an import's chunk wrote since its last batch was logged.
// A nil one logs nothing.
type madeTasks struct {
	by      string // who runs the import
	changes []workFeed.TaskChange
	open    []uuid.UUID // the tasks made open: a batch of only those reads no restriction
}

func newMadeTasks(importer string) *madeTasks { return &madeTasks{by: importer} }

// add is a task writeTask stored, as the write leaves it: its assignee as
// lists show people, and its parent by its id and name.
func (m *madeTasks) add(in importTaskInput, task *dgraphStruct.DgraphTask) {
	if m == nil {
		return
	}
	v := *task
	v.Assignee, v.ParentTask = in.Assignee, nil
	if p := in.ParentTask; p != nil {
		v.ParentTask = &dgraphStruct.DgraphTask{Uuid: p.Uuid, Name: p.Name}
	}
	m.changes = append(m.changes, workFeed.TaskChange{Kind: workFeed.KindCreated, Project: in.ProjectUUID.String(), Task: &v, Fields: workFeed.TaskBasics})
	if id, err := uuid.Parse(v.Uuid); err == nil && in.Restricted == "" {
		m.open = append(m.open, id)
	}
}

// log logs the batch, once, and starts the next.
func (m *madeTasks) log(ctx context.Context) {
	if m == nil || len(m.changes) == 0 {
		return
	}
	workFeed.RecordTasks(helpers.WithOpenTasks(ctx, m.open...), m.by, m.changes...)
	m.changes, m.open = nil, nil
}
