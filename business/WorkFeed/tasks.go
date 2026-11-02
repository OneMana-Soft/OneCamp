package business

// The one funnel from task writes into the Work Change Log (WCL-1):
// business/Task and business/TaskField call RecordTasks once per write, after
// the write is stored.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// What happened to a task.
const (
	KindCreated  = "task.created"
	KindUpdated  = "task.updated"
	KindDeleted  = "task.deleted"
	KindRestored = "task.restored"
)

// TaskBasics are the fields a made or restored task's entry carries: what a
// list or a board shows of it.
var TaskBasics = []string{"task_name", "task_status", "task_custom_status", "task_custom_status_name",
	"task_status_since", "task_priority", "task_label", "task_start_date", "task_due_date", "task_assignee",
	"task_rank", "task_estimate_minutes", "task_estimate_points", "task_parent_task"}

// TaskMade are the fields a made task's entry carries: its basics and its
// description, which task.created has always sent (business/Task
// taskWebhooks.go reads the event from the entry).
var TaskMade = append(append([]string(nil), TaskBasics...), "task_description")

// TaskGone are the fields a deleted task's entry carries: when it went, and
// the name and status task.deleted has always sent.
var TaskGone = []string{"task_deleted_at", "task_name", "task_status"}

// TaskChange is one change to a task as its write reports it: what happened,
// its project, the task as it now is, and which of its fields changed, named
// as the app's task answer names them. Diff, when set, is carried as it is
// instead: a field value, which the task answer doesn't hold.
type TaskChange struct {
	Kind    string
	Project string
	Task    *dgraphStruct.DgraphTask
	Fields  []string
	Diff    json.RawMessage
}

// restrictedTasks is which of the tasks, by lower-case uuid, are kept to a
// few people (WA2), in one read. A read that fails counts every one: the
// feed then shows them only to those let in. A seam for tests.
var restrictedTasks = func(ctx context.Context, ids []string) map[string]bool {
	parsed := make([]uuid.UUID, 0, len(ids))
	for _, s := range ids {
		if id, err := uuid.Parse(s); err == nil {
			parsed = append(parsed, id)
		}
	}
	found, err := restrictionModel.OfMany(ctx, parsed)
	out := make(map[string]bool, len(parsed))
	for _, id := range parsed {
		if err != nil || found[id].Restricted {
			out[id.String()] = true
		}
	}
	return out
}

// RecordTasks is the one funnel from task writes into the log. Every write
// in business/Task and business/TaskField that changes a task calls it once,
// after the write is stored, so a change from any source (the app, the API,
// MCP, rules, forms, agents) is logged exactly once. by is who made it (a
// user uuid; "" takes the request's reader). A change naming no project or
// task is nobody's to read and isn't logged. It never fails the write, and
// a caller gone before it ends doesn't stop it.
func RecordTasks(ctx context.Context, by string, changes ...TaskChange) []Entry {
	ctx = context.WithoutCancel(ctx)
	if by == "" {
		by = taskread.ReaderOf(ctx)
	}
	if _, err := uuid.Parse(by); err != nil {
		by = ""
	}
	now, source := time.Now(), sourceOf(ctx)
	out := make([]Change, 0, len(changes))
	ids := make([]string, 0, len(changes))
	for _, c := range changes {
		if c.Task == nil || !isUUID(c.Task.Uuid) || !isUUID(c.Project) {
			continue
		}
		diff := c.Diff
		if diff == nil {
			diff = TaskDiff(c.Task, c.Fields...)
		}
		out = append(out, Change{Kind: c.Kind, ResourceKind: "task", ResourceUUID: c.Task.Uuid, ProjectUUID: c.Project,
			Diff: diff, Actor: by, Source: source, At: now})
		ids = append(ids, c.Task.Uuid)
	}
	if len(out) == 0 {
		return nil
	}
	// Tasks a batch just made outside any restricted task are open
	// (helpers.WithOpenTasks): nothing to read.
	if !helpers.TasksOpen(ctx, ids...) {
		restricted := restrictedTasks(ctx, ids)
		for i := range out {
			out[i].Restricted = restricted[strings.ToLower(out[i].ResourceUUID)]
		}
	}
	return Record(ctx, out...)
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// TaskDiff is the named fields of t as the app's task answer carries them
// (DgraphTask's own JSON names and values), null for one t leaves unset, so
// a cleared value reads as cleared. Someone it names is carried as lists
// show them (id, names and picture), never with their email; a parent task
// by its id and name.
func TaskDiff(t *dgraphStruct.DgraphTask, fields ...string) json.RawMessage {
	if t == nil || len(fields) == 0 {
		return nil
	}
	v := *t
	if a := v.Assignee; a != nil && a.Uuid != "" {
		v.Assignee = &dgraphStruct.DgraphUser{Uuid: a.Uuid, UserName: a.UserName, UserFullName: a.UserFullName, ProfileKey: a.ProfileKey}
	} else {
		v.Assignee = nil
	}
	if p := v.ParentTask; p != nil {
		v.ParentTask = &dgraphStruct.DgraphTask{Uuid: p.Uuid, Name: p.Name}
	}
	whole, err := json.Marshal(&v)
	if err != nil {
		return nil
	}
	var all map[string]json.RawMessage
	if json.Unmarshal(whole, &all) != nil {
		return nil
	}
	picked := make(map[string]json.RawMessage, len(fields))
	for _, f := range fields {
		if raw, ok := all[f]; ok {
			picked[f] = raw
		} else {
			picked[f] = json.RawMessage("null")
		}
	}
	out, err := json.Marshal(picked)
	if err != nil {
		return nil
	}
	return out
}

// sourceOf is where a write came from: an agent's run, an API token (the
// public API or MCP), or the app.
func sourceOf(ctx context.Context) string {
	switch {
	case helpers.IsAgentWrite(ctx):
		return "agent"
	case helpers.ActsThroughToken(ctx):
		return "api"
	}
	return "app"
}
