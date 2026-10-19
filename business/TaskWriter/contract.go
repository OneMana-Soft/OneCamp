// Package business (TaskWriter) is the Task Writer
// (design/projects-vs-asana-jira/PROGRAM.md §4.2 #87 and PJ1; A3 builds it):
// the one way a task changes, from any source — the app, the public API,
// MCP, agents, GitHub, imports, forms, rules, Slack and email. Each change is
// validated, versioned per field, authorised once through Work Access, and
// written with its numbered entry in the Work Change Log (#88).
//
// DAY 0. Apply makes today's changes the way today's handlers make them: the
// person must be able to change the task, or for a deletion delete it
// (business/Task TaskFor, Work Access's one check), and each op is the
// business function its handler calls. Only the ops whose whole rule already lives in those functions are
// here: status, estimate, delete and restore. The others answer
// ErrUnavailable, because their rules still live in the handlers
// (controllers/Task), and a copy here would be a second rule: A3 moves them
// behind this interface and makes the handlers thin adapters over it.
// Versions are ignored and nothing is logged yet. Nothing calls Apply, so
// nothing changes by its existing.
package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// OpKind is what one op changes. Append-only after A3 merges (ops.go).
type OpKind string

const (
	OpName        OpKind = "name"
	OpDescription OpKind = "description"
	OpStatus      OpKind = "status"
	OpAssignee    OpKind = "assignee"
	OpPriority    OpKind = "priority"
	OpTags        OpKind = "tags"
	OpStart       OpKind = "start"
	OpDue         OpKind = "due"
	OpEstimate    OpKind = "estimate"
	OpField       OpKind = "field"
	OpParent      OpKind = "parent"
	OpRelation    OpKind = "relation"
	OpRank        OpKind = "rank"
	OpCycle       OpKind = "cycle"
	OpProject     OpKind = "project"
	OpDelete      OpKind = "delete"
	OpRestore     OpKind = "restore"
)

// Op is one change to a task: its kind and its new value, as JSON (a status's
// key or name, an estimate's minutes).
type Op struct {
	Kind  OpKind          `json:"op"`
	Value json.RawMessage `json:"value,omitempty"`
}

// Change is what one op did, for the change log A3 writes (sequence, actor,
// source and the field's versions come with it). Day 0 names the op only.
type Change struct {
	Op OpKind `json:"op"`
}

// Writer changes tasks.
type Writer interface {
	// Apply makes ops to the task, in order, as who. baseVersion is the
	// version the person saw (A3 refuses a stale one with the current value).
	Apply(ctx context.Context, who *userModels.UserInfo, taskID uuid.UUID, ops []Op, baseVersion int64) (*dgraphStruct.DgraphTask, []Change, error)
}

// ErrUnavailable is an op the default doesn't make yet: A3 brings it.
var ErrUnavailable = errors.New("tasks: that change isn't available here yet")

// ErrBadValue is an op whose value isn't one its kind takes.
var ErrBadValue = errors.New("tasks: that value doesn't fit")

// Tasks is the writer every source calls.
var Tasks Writer = todaysWrites{}

// Seams for tests: today's check and business functions.
var (
	taskFor     = taskBusiness.TaskFor
	setStatus   = taskBusiness.UpdateTaskStatusByTaskUUID
	setEstimate = taskBusiness.UpdateTaskEstimate
	deleteTask  = taskBusiness.ArchiveTaskByTaskUUID
	restoreTask = taskBusiness.UnArchiveTaskByTaskUUID
)

// todaysWrites makes the ops today's handlers make, with their check.
type todaysWrites struct{}

// made is the ops the default makes.
var made = map[OpKind]bool{OpStatus: true, OpEstimate: true, OpDelete: true, OpRestore: true}

func (todaysWrites) Apply(ctx context.Context, who *userModels.UserInfo, taskID uuid.UUID, ops []Op, _ int64) (*dgraphStruct.DgraphTask, []Change, error) {
	if who == nil {
		return nil, nil, taskBusiness.ErrTaskNotVisible
	}
	// Every op is one the default makes, or none is made.
	for _, op := range ops {
		if !made[op.Kind] {
			return nil, nil, fmt.Errorf("%w: %s", ErrUnavailable, op.Kind)
		}
	}
	// One check for the batch, by the most it asks (deleting asks more of a
	// member than changing: only the tasks they made).
	need := workAccess.NeedEditTask
	for _, op := range ops {
		if op.Kind == OpDelete || op.Kind == OpRestore {
			need = workAccess.NeedDeleteTask
		}
	}
	task, _, err := taskFor(ctx, taskID, who, need)
	if err != nil {
		return nil, nil, err
	}
	if need == workAccess.NeedDeleteTask && len(ops) > 1 {
		if _, _, err := taskFor(ctx, taskID, who, workAccess.NeedEditTask); err != nil {
			return nil, nil, err
		}
	}
	me := &who.UserDgraphInfo
	changes := make([]Change, 0, len(ops))
	for _, op := range ops {
		var err error
		switch op.Kind {
		case OpStatus:
			var status string
			if json.Unmarshal(op.Value, &status) != nil || status == "" {
				return task, changes, fmt.Errorf("%w: a status is its key or name", ErrBadValue)
			}
			err = setStatus(ctx, taskID, status, task, me)
		case OpEstimate:
			var minutes int
			if json.Unmarshal(op.Value, &minutes) != nil {
				return task, changes, fmt.Errorf("%w: an estimate is whole minutes", ErrBadValue)
			}
			err = setEstimate(ctx, taskID, minutes, task, me)
		case OpDelete:
			err = deleteTask(ctx, taskID, task, me)
		case OpRestore:
			err = restoreTask(ctx, taskID, task, me)
		}
		if err != nil {
			return task, changes, err
		}
		changes = append(changes, Change{Op: op.Kind})
	}
	return task, changes, nil
}
