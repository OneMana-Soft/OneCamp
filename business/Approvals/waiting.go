package business

// "Waiting for my approval" (package 9 R2b): every open request that names
// the reader as an approver and that they haven't answered, newest first.
// Each item is read as the reader, so a task they can no longer see (moved
// out of their reach, or restricted) never shows; the list is short-lived
// state read from Postgres, not the work graph.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// WaitingLimit bounds the list.
const WaitingLimit = 50

// Waiting is one request waiting for the reader's answer.
type Waiting struct {
	ApprovalID  uuid.UUID  `json:"approval_id"`
	Kind        string     `json:"kind"`
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Path        string     `json:"path"`
	ToName      string     `json:"to_name,omitempty"`
	Ask         string     `json:"ask,omitempty"`
	DueAt       *time.Time `json:"due_at,omitempty"`
	RequestedBy uuid.UUID  `json:"requested_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ProjectID   string     `json:"project_id,omitempty"`
	ProjectName string     `json:"project_name,omitempty"`
	// Asker is who asked, as anyone may be shown them: a bot says so.
	Asker *dgraphStruct.DgraphUser `json:"asker,omitempty"`
}

// Seams for tests: the requests that name a person, and the people who asked.
var (
	waitingFor = model.Waiting
	peopleOf   = userDomain.GetPeopleByUUIDs
)

// WaitingFor is who's requests to answer, each read as them.
func WaitingFor(ctx context.Context, who *userModels.UserInfo) ([]Waiting, error) {
	open, err := waitingFor(ctx, who.UserPostgresInfo.Id, WaitingLimit)
	if err != nil {
		return nil, err
	}
	out := make([]Waiting, 0, len(open))
	for _, a := range open {
		item := Waiting{ApprovalID: a.ID, Kind: a.ResourceKind, ID: a.ResourceID, Ask: a.Ask, DueAt: a.DueAt, RequestedBy: a.RequestedBy, CreatedAt: a.CreatedAt}
		switch a.ResourceKind {
		case model.KindTask:
			task, _, err := taskFor(ctx, a.TaskID, who, workAccess.NeedView)
			if err != nil {
				if !errors.Is(err, taskBusiness.ErrTaskNotVisible) {
					helpers.LogErrorWithContext(ctx, "Approvals: waiting for %s, task %s: %v", who.UserPostgresInfo.Id, a.TaskID, err)
				}
				continue
			}
			item.Title, item.Path = task.Name, "/app/task/"+task.Uuid
			if task.Project != nil {
				item.ProjectID, item.ProjectName = task.Project.Uuid, task.Project.Name
			}
			if a.ToStatus != "" {
				item.ToName = statusName(ctx, a.ProjectID, a.ToStatus)
			}
		default:
			// Docs and files come with their own readers (R2b-1b).
			continue
		}
		out = append(out, item)
	}
	nameAskers(ctx, out)
	return out, nil
}

// nameAskers puts who asked on each item, in one read.
func nameAskers(ctx context.Context, items []Waiting) {
	if len(items) == 0 {
		return
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.RequestedBy.String())
	}
	askers := whoAsked(ctx, ids)
	for i := range items {
		items[i].Asker = askers[items[i].RequestedBy.String()]
	}
}

// whoAsked is the people who asked, by id, as anyone may be shown them, in
// one read. A list without them still lets its reader answer, so a failed
// read is logged and names nobody.
func whoAsked(ctx context.Context, ids []string) map[string]*dgraphStruct.DgraphUser {
	people, err := peopleOf(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Approvals: who asked: %v", err)
		return nil
	}
	byID := make(map[string]*dgraphStruct.DgraphUser, len(people))
	for _, p := range people {
		if p != nil {
			byID[strings.ToLower(p.Uuid)] = p.Public()
		}
	}
	return byID
}
