package business

// Typed links between tasks (jira-01 J-H; the store is domain/Task
// taskLinkDomain.go): a task relates to, duplicates or clones another.
// Blocking is not a link: it is the date dependency (AddTaskDependency,
// finish to start and the rest), and a link is refused as "blocks".

import (
	"context"
	"errors"
	"fmt"
	"strings"

	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	domain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// ErrLink is a link that can't be made that way, in words.
var ErrLink = domain.ErrLink

// MaxLinks is how many live links a task holds, both ways together.
const MaxLinks = domain.MaxLinks

// LinkKind is nil for a kind of link (relates, duplicates, clones); "blocks"
// is refused in words pointing to dependencies.
var LinkKind = domain.LinkKind

// Seam for tests: the links' read.
var readLinks = domain.ReadLinks

// LinkAsk is one link asked for from a task's side: its kind, the task on
// its other end, and with Direction "in" that task as its from end (it
// duplicates this one); Remove takes it off.
type LinkAsk struct {
	Kind      string
	Other     string
	Direction string
	Remove    bool
}

// TaskLink is one of a task's links as its reader reads it.
type TaskLink struct {
	Kind      string     `json:"kind"`
	Direction string     `json:"direction"`
	Label     string     `json:"label"`
	Task      LinkedTask `json:"task"`
}

// LinkedTask is the task on a link's other end: only ever one the reader
// may see.
type LinkedTask struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	Key         string `json:"key,omitempty"`
	Type        string `json:"type,omitempty"`
	Status      string `json:"status"`
	ProjectUUID string `json:"project_uuid"`
}

// ChangeLink adds, or with ask.Remove takes off, a link from the task's
// side, as user (domain ChangeLink): both ids are read in the change's own
// transaction as live tasks, and the person must be able to change the task
// and see the other, by Work Access's link rule (as a dependency's: in
// another project, change tasks there too; an other task they can't see is
// not found, nothing says it exists). A new link is refused once either task
// holds MaxLinks. A removed link is kept, stamped with who removed it.
func ChangeLink(ctx context.Context, taskID uuid.UUID, ask LinkAsk, user *dgraphStruct.DgraphUser) error {
	from, to, key, err := domain.LinkEnds(ask.Kind, taskID.String(), ask.Other, ask.Direction)
	if err != nil {
		return err
	}
	mine := strings.ToLower(taskID.String())
	_, err = domain.ChangeLink(ctx, from, to, key, ask.Kind, user.Uid, ask.Remove, func(c *domain.LinkChange) error {
		task, other := c.From, c.To
		taskPlace, otherPlace := c.FromPlace, c.ToPlace
		if from != mine {
			task, other, taskPlace, otherPlace = other, task, otherPlace, taskPlace
		}
		if task == nil || taskPlace == nil || task.Project == nil || other == nil || otherPlace == nil || other.Project == nil {
			return ErrTaskNotVisible
		}
		if err := mayLinkInTxn(ctx, task, other, user); err != nil {
			if errors.Is(err, ErrDependencyMissing) {
				return ErrTaskNotVisible
			}
			return err
		}
		adding := !ask.Remove && (c.Node == nil || !c.Node.Live)
		if adding && (c.Held[from] >= MaxLinks || c.Held[to] >= MaxLinks) {
			return domain.LinkRefusal(fmt.Sprintf("A task holds at most %d links: remove one first.", MaxLinks))
		}
		return nil
	})
	return err
}

// Links is the task's live links, both ways, at most MaxLinks, as who reads
// them: the task must be one they may see (else ErrTaskNotVisible), and a
// task on a link's other end they may not see now (restricted, deleted, in a
// project they aren't in) is left out whole and not counted. Each other task
// carries its key and type (TaskKeys.Lay).
func Links(ctx context.Context, taskID uuid.UUID, who *model.UserInfo) ([]TaskLink, error) {
	task, _, err := TaskFor(ctx, taskID, who, workAccess.NeedView)
	if err != nil {
		return nil, err
	}
	ctx = taskread.ActingAs(ctx, *who)
	rows, err := readLinks(ctx, task.Uuid, who.UserDgraphInfo.Uid)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	ends := make([]*dgraphStruct.DgraphTask, len(rows))
	for i, r := range rows {
		ends[i] = r.Task
	}
	verdicts, err := workAccess.JudgeTasks(ctx, who.UserDgraphInfo.Uuid, who.UserDgraphInfo.Uid, ends...)
	if err != nil {
		return nil, err
	}
	kept := rows[:0]
	shown := ends[:0]
	for _, r := range rows {
		if verdicts[strings.ToLower(r.Task.Uuid)].See {
			kept = append(kept, r)
			shown = append(shown, r.Task)
		}
	}
	taskKeys.Lay(ctx, shown...)
	out := make([]TaskLink, 0, len(kept))
	for _, r := range kept {
		t := r.Task
		l := LinkedTask{UUID: t.Uuid, Name: t.Name, Key: t.Key, Status: t.Status, ProjectUUID: t.Project.Uuid}
		if t.WorkType != nil {
			l.Type = t.WorkType.Name
		}
		out = append(out, TaskLink{Kind: r.Kind, Direction: r.Direction, Label: domain.LinkLabel(r.Kind, r.Direction), Task: l})
	}
	return out, nil
}
