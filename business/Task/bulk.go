package business

// One change to many tasks (PR2, the BE audit's bulk writes): the list's and
// the board's selection, the API, agents. Every task is read in one query
// and checked in one pass (Work Access, restricted tasks included); a
// priority or a tag is then written for the lot with one Postgres statement
// and one graph mutation, each task keeping its own activity line. A status,
// a due date, the trash and back go task by task through the writer each has
// (their side effects: recurrence, dependents, calendars, the trash), still in
// one request.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	syncBusiness "github.com/akashc777/OneCamp/business"
	taskPriority "github.com/akashc777/OneCamp/business/TaskPriority"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// BulkMost is the most tasks one bulk change takes.
const BulkMost = 500

// The changes a bulk request makes.
const (
	BulkPriority  = "priority"
	BulkTagAdd    = "tag_add"
	BulkTagRemove = "tag_remove"
	BulkStatus    = "status"
	BulkDue       = "due"
	BulkDelete    = "delete"
	BulkRestore   = "restore"
)

// BulkChange is one change: what (Op) and to what (Value: a priority, a tag,
// a status, a due date as RFC 3339 or "" for none).
type BulkChange struct {
	Op    string `json:"op"`
	Value string `json:"value"`
}

// BulkRefusal is a task the change wasn't made to, and why, in words.
type BulkRefusal struct {
	TaskUUID string `json:"task_uuid"`
	Message  string `json:"msg"`
}

// BulkResult is how a bulk change went, task by task.
type BulkResult struct {
	Changed   []string      `json:"changed"`
	Unchanged []string      `json:"unchanged"`
	Refused   []BulkRefusal `json:"refused"`
}

// BulkError is a request that can't be made at all; its text is for the person.
type BulkError struct{ msg string }

func (e *BulkError) Error() string { return e.msg }

// bulkSeams are where a bulk change reads, checks and writes: seams for tests.
var bulkSeams = struct {
	read       func(ctx context.Context, ids []string, userUID string) ([]*dgraphStruct.DgraphTask, error)
	check      func(ctx context.Context, who *model.UserInfo, ts []*dgraphStruct.DgraphTask, need workAccess.Need) map[string]error
	touch      func(ctx context.Context, ids []uuid.UUID, at time.Time) error
	write      func(ctx context.Context, ts []*dgraphStruct.DgraphTask) error
	index      func(ts []*dgraphStruct.DgraphTask)
	githubSync func(ctx context.Context, taskID uuid.UUID, syncType string, payload map[string]interface{})
	one        func(ctx context.Context, t *dgraphStruct.DgraphTask, change BulkChange, who *dgraphStruct.DgraphUser) error
	visible    func(t *dgraphStruct.DgraphTask, userUID string) bool
}{
	visible:    CanViewTask,
	read:       domain.GetDgraphTasksForWrite,
	check:      checkTasks,
	touch:      domain.TouchTasks,
	write:      domain.SetDgraphTasks,
	index:      indexTasks,
	githubSync: syncBusiness.EnqueueGitHubSync,
	one:        changeOne,
}

// checkTasks is what stops the person (who) from need on each of ts, by
// lower-case task uuid; a task missing from the map they may do it to. One
// read of their roles and the tasks' restrictions however many tasks.
func checkTasks(ctx context.Context, who *model.UserInfo, ts []*dgraphStruct.DgraphTask, need workAccess.Need) map[string]error {
	out := map[string]error{}
	var projects []*dgraphStruct.DgraphProject
	for _, t := range ts {
		if t.Project != nil {
			projects = append(projects, t.Project)
		}
	}
	standings, err := workAccess.OfMany(ctx, who.UserDgraphInfo.Uuid, projects)
	if err == nil {
		var facts map[string]workAccess.TaskFacts
		facts, err = workAccess.FactsForMany(ctx, who.UserDgraphInfo.Uuid, who.UserDgraphInfo.Uid, ts...)
		if err == nil {
			for _, t := range ts {
				var s workAccess.Standing
				if t.Project != nil {
					s = standings[strings.ToLower(t.Project.Uuid)]
				}
				if e := s.CheckTask(need, facts[strings.ToLower(t.Uuid)]); e != nil {
					out[strings.ToLower(t.Uuid)] = e
				}
			}
			return out
		}
	}
	for _, t := range ts {
		out[strings.ToLower(t.Uuid)] = err
	}
	return out
}

func indexTasks(ts []*dgraphStruct.DgraphTask) {
	docs := make([]*openSearchStruct.OpenSearchTask, 0, len(ts))
	for _, t := range ts {
		os := &openSearchStruct.OpenSearchTask{Uuid: t.Uuid, TaskPriority: t.Priority, TaskLabel: t.Label}
		if t.UpdatedAt != nil {
			os.TaskUpdatedAt = t.UpdatedAt.Unix()
		}
		docs = append(docs, os)
	}
	domain.UpdateTasksInOpenSearch(docs)
}

// valid checks a change can be made at all, and words what can't.
func (c BulkChange) valid() error {
	switch c.Op {
	case BulkPriority:
		if !taskPriority.IsOrNone(c.Value) {
			return &BulkError{fmt.Sprintf("%q isn't a priority.", c.Value)}
		}
	case BulkTagAdd, BulkTagRemove:
		if strings.TrimSpace(helpers.NormaliseTags(c.Value)) == "" {
			return &BulkError{"Name the tag."}
		}
	case BulkStatus:
		if strings.TrimSpace(c.Value) == "" {
			return &BulkError{"Name the status."}
		}
	case BulkDue:
		if c.Value != "" {
			if _, err := time.Parse(time.RFC3339, c.Value); err != nil {
				return &BulkError{"That due date couldn't be read."}
			}
		}
	case BulkDelete, BulkRestore:
	default:
		return &BulkError{"That isn't a change a bulk request makes."}
	}
	return nil
}

func (c BulkChange) need() workAccess.Need {
	if c.Op == BulkDelete || c.Op == BulkRestore {
		return workAccess.NeedDeleteTask
	}
	return workAccess.NeedEditTask
}

// Bulk makes change to the tasks with ids, for who. A task they can't see is
// refused as not found, one they may not change in the words Work Access
// gives, a deleted one (but for a restore) as deleted; one already so is
// unchanged. The error is for a request that can't be made at all (BulkError)
// or a read that failed.
func Bulk(ctx context.Context, who *model.UserInfo, ids []uuid.UUID, change BulkChange) (*BulkResult, error) {
	if len(ids) > BulkMost {
		return nil, &BulkError{fmt.Sprintf("Change at most %d tasks at a time.", BulkMost)}
	}
	if err := change.valid(); err != nil {
		return nil, err
	}
	res := &BulkResult{Changed: []string{}, Unchanged: []string{}, Refused: []BulkRefusal{}}
	asked := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if s := id.String(); !seen[s] {
			seen[s] = true
			asked = append(asked, s)
		}
	}
	if len(asked) == 0 {
		return res, nil
	}
	tasks, err := bulkSeams.read(ctx, asked, who.UserDgraphInfo.Uid)
	if err != nil {
		return nil, err
	}
	found := map[string]*dgraphStruct.DgraphTask{}
	var seenTasks []*dgraphStruct.DgraphTask
	for _, t := range tasks {
		// One they can't see (outside its project) is as one that isn't there.
		if t != nil && t.Uuid != "" && bulkSeams.visible(t, who.UserDgraphInfo.Uid) {
			found[strings.ToLower(t.Uuid)] = t
			seenTasks = append(seenTasks, t)
		}
	}
	tasks = seenTasks
	refusals := bulkSeams.check(ctx, who, tasks, change.need())
	var allowed []*dgraphStruct.DgraphTask
	for _, id := range asked {
		t := found[id]
		switch {
		case t == nil:
			res.Refused = append(res.Refused, BulkRefusal{id, "That task doesn't exist or you can't see it."})
		case refusals[id] != nil:
			res.Refused = append(res.Refused, BulkRefusal{id, refusalWords(refusals[id])})
		case change.Op != BulkRestore && helpers.IsSoftDeleted(t.DeletedAt):
			res.Refused = append(res.Refused, BulkRefusal{id, "That task was deleted."})
		default:
			allowed = append(allowed, t)
		}
	}
	switch change.Op {
	case BulkPriority, BulkTagAdd, BulkTagRemove:
		err = bulkFields(ctx, allowed, change, &who.UserDgraphInfo, res)
	default:
		for _, t := range allowed {
			if e := bulkSeams.one(ctx, t, change, &who.UserDgraphInfo); e != nil {
				res.Refused = append(res.Refused, BulkRefusal{strings.ToLower(t.Uuid), oneWords(e)})
				continue
			}
			res.Changed = append(res.Changed, strings.ToLower(t.Uuid))
		}
	}
	return res, err
}

func refusalWords(err error) string {
	var r *workAccess.Refusal
	if errors.As(err, &r) {
		return r.Words()
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		if msg := err.Error(); msg != "" && !strings.Contains(msg, ":") {
			return msg
		}
	}
	return "Couldn't check what you may do with that task. Try again."
}

func oneWords(err error) string {
	switch {
	case errors.Is(err, ErrTaskDeleted):
		return "That task was deleted."
	default:
		var ce interface{ UserMessage() string }
		if errors.As(err, &ce) {
			return ce.UserMessage()
		}
		return "Couldn't change that task. Try again."
	}
}

// bulkFields writes a priority or a tag to tasks: each task's own new value
// and activity line, one Postgres statement and one graph mutation for all.
func bulkFields(ctx context.Context, tasks []*dgraphStruct.DgraphTask, change BulkChange, by *dgraphStruct.DgraphUser, res *BulkResult) error {
	now := time.Now()
	zero := time.Time{}
	var writes []*dgraphStruct.DgraphTask
	var ids []uuid.UUID
	type tagSync struct {
		id       uuid.UUID
		from, to string
	}
	var tags []tagSync
	for _, t := range tasks {
		id, err := uuid.Parse(t.Uuid)
		if err != nil {
			continue
		}
		w := &dgraphStruct.DgraphTask{Uid: t.Uid, Uuid: t.Uuid, UpdatedAt: &now}
		act := &dgraphStruct.DgraphTaskActivity{Uuid: uuid.NewString(), CreatedBy: &dgraphStruct.DgraphUser{Uid: by.Uid}, LogTime: &now}
		switch change.Op {
		case BulkPriority:
			if t.Priority == change.Value {
				res.Unchanged = append(res.Unchanged, strings.ToLower(t.Uuid))
				continue
			}
			w.Priority, w.DeletedAt = change.Value, &zero
			act.Type, act.PrevState, act.NextState = dgraphStruct.ACTIVITY_TYPE_PRIORITY, t.Priority, change.Value
		default:
			from := ""
			if t.Label != nil {
				from = *t.Label
			}
			to := helpers.WithTag(from, change.Value)
			if change.Op == BulkTagRemove {
				to = helpers.WithoutTag(from, change.Value)
			}
			to = helpers.NormaliseTags(to)
			if to == helpers.NormaliseTags(from) {
				res.Unchanged = append(res.Unchanged, strings.ToLower(t.Uuid))
				continue
			}
			w.Label = &to
			act.Type, act.PrevState, act.NextState = dgraphStruct.ACTIVITY_TYPE_LABEL, from, to
			tags = append(tags, tagSync{id, from, to})
		}
		w.Activity = []*dgraphStruct.DgraphTaskActivity{act}
		writes = append(writes, w)
		ids = append(ids, id)
	}
	if len(writes) == 0 {
		return nil
	}
	if err := bulkSeams.touch(ctx, ids, now); err != nil {
		return err
	}
	if err := bulkSeams.write(ctx, writes); err != nil {
		return err
	}
	for _, w := range writes {
		res.Changed = append(res.Changed, strings.ToLower(w.Uuid))
	}
	// Search and GitHub follow, as for a single change, off the request.
	index, githubSync := bulkSeams.index, bulkSeams.githubSync
	helpers.GoSafeNamed("Task.Bulk:index", func() { index(writes) })
	for _, s := range tags {
		helpers.GoSafeNamed("Task.Bulk:githubSync", func() {
			githubSync(ctx, s.id, "label", map[string]interface{}{"label": s.to, "old_label": s.from})
		})
	}
	return nil
}

// changeOne makes a change that has a writer of its own to one task.
func changeOne(ctx context.Context, t *dgraphStruct.DgraphTask, change BulkChange, by *dgraphStruct.DgraphUser) error {
	id, err := uuid.Parse(t.Uuid)
	if err != nil {
		return err
	}
	switch change.Op {
	case BulkStatus:
		return UpdateTaskStatusByTaskUUID(ctx, id, change.Value, t, by)
	case BulkDue:
		var due *time.Time
		if change.Value != "" {
			d, _ := time.Parse(time.RFC3339, change.Value)
			due = &d
		}
		return UpdateTaskDueDateByTaskUUID(ctx, id, due, t, by)
	case BulkDelete:
		if helpers.IsSoftDeleted(t.DeletedAt) {
			return nil
		}
		return ArchiveTaskByTaskUUID(ctx, id, t, by)
	case BulkRestore:
		if !helpers.IsSoftDeleted(t.DeletedAt) {
			return nil
		}
		return UnArchiveTaskByTaskUUID(ctx, id, t, by)
	}
	return &BulkError{"That isn't a change a bulk request makes."}
}

// ChangeRefusals is, for each of ids the person (who) may not do need to,
// why, in their words; a task missing from the map they may. One read of the
// tasks and one check, however many (PR2, the BE audit's #13: GitHub's bulk
// link and unlink asked a full single-task read and check per task).
func ChangeRefusals(ctx context.Context, who *model.UserInfo, ids []uuid.UUID, need workAccess.Need) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	asked := make([]string, 0, len(ids))
	for _, id := range ids {
		asked = append(asked, id.String())
	}
	tasks, err := bulkSeams.read(ctx, asked, who.UserDgraphInfo.Uid)
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	var seenTasks []*dgraphStruct.DgraphTask
	for _, t := range tasks {
		if t != nil && bulkSeams.visible(t, who.UserDgraphInfo.Uid) {
			found[strings.ToLower(t.Uuid)] = true
			seenTasks = append(seenTasks, t)
		}
	}
	tasks = seenTasks
	refusals := bulkSeams.check(ctx, who, tasks, need)
	for _, id := range ids {
		key := id.String()
		switch {
		case !found[key]:
			out[id] = "That task doesn't exist or you can't see it."
		case refusals[key] != nil:
			out[id] = refusalWords(refusals[key])
		}
	}
	return out, nil
}
