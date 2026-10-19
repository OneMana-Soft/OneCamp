package business

// What an empty doc's AI row offers (docs programme B9, small-details E8):
// "Ask AI to draft", and "Draft from this project's open tasks" when the doc
// is in a project the person is in, with the number of open tasks, so the
// offer says what it will read. Notion has no idea what a project's tasks are.

import (
	"context"
	"fmt"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// DocProject is the project a doc is in, as its starters show it.
type DocProject struct {
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	OpenTasks int    `json:"open_tasks"`
}

// DocAIStartersResult is what an empty doc can offer.
type DocAIStartersResult struct {
	// Project is the doc's project when the person is in it; nil otherwise.
	Project *DocProject `json:"project"`
}

// openTasksMax is how many open tasks a draft reads.
const openTasksMax = 60

// Seams over the graph, for tests.
var (
	docSpaceOf = func(ctx context.Context, docUUID, viewerUID string) (kind, spaceUUID string) {
		rows, err := docDomain.GetDocRowsByUUID(ctx, []string{docUUID}, viewerUID)
		if err != nil || len(rows) == 0 || rows[0] == nil {
			return "", ""
		}
		return rows[0].SpaceKind, rows[0].SpaceUUID
	}
	projectOpenTasks = func(ctx context.Context, projectUUID, viewerUID string) (*dgraphStruct.DgraphProject, error) {
		return projectBusiness.GetDgraphProjectTaskList(ctx, projectUUID, viewerUID, dgraphStruct.TASK_OPEN_FILTER, "orderasc: task_due_date", openTasksMax, 0, false)
	}
)

// DocAIStarters is what an empty doc's AI row offers u, an editor of the doc.
func DocAIStarters(ctx context.Context, u *userModels.UserInfo, docUUID string) (DocAIStartersResult, error) {
	if _, err := checkDocQuietly(ctx, u, docUUID, accessBusiness.LevelEdit); err != nil {
		return DocAIStartersResult{}, err
	}
	return DocAIStartersResult{Project: docProjectFor(ctx, u, docUUID)}, nil
}

// docProjectFor is the project a doc is in, when u is in it; nil otherwise.
// It checks no access to the doc: its callers have.
func docProjectFor(ctx context.Context, u *userModels.UserInfo, docUUID string) *DocProject {
	kind, space := docSpaceOf(ctx, docUUID, u.UserDgraphInfo.Uid)
	if kind != "project" || space == "" {
		return nil
	}
	p, err := projectOpenTasks(ctx, space, u.UserDgraphInfo.Uid)
	if err != nil || p == nil || p.Uuid == "" || !p.ReaderIsIn() {
		return nil
	}
	return &DocProject{UUID: p.Uuid, Name: strings.TrimSpace(p.Name), OpenTasks: int(p.TaskCount)}
}

// ErrNotInProject is a project the person isn't in, or that doesn't exist.
var ErrNotInProject = fmt.Errorf("you aren't in that project, so its tasks can't be read")

// ProjectOpenTasksText is a project's open tasks as lines a model reads, for a
// person who is in the project: its name, the lines, and how many there are.
func ProjectOpenTasksText(ctx context.Context, u *userModels.UserInfo, projectUUID string) (string, string, int, error) {
	if _, err := uuid.Parse(strings.TrimSpace(projectUUID)); err != nil {
		return "", "", 0, ErrNotInProject
	}
	p, err := projectOpenTasks(ctx, projectUUID, u.UserDgraphInfo.Uid)
	if err != nil {
		return "", "", 0, err
	}
	if p == nil || p.Uuid == "" || !p.ReaderIsIn() {
		return "", "", 0, ErrNotInProject
	}
	var b strings.Builder
	for _, t := range p.Tasks {
		if t == nil {
			continue
		}
		b.WriteString(openTaskLine(t))
		b.WriteString("\n")
	}
	return strings.TrimSpace(p.Name), strings.TrimSpace(b.String()), len(p.Tasks), nil
}

// openTaskLine is one open task for a draft: its name, status, owner and due
// date, and no ids (a draft is read by people). Pure.
func openTaskLine(t *dgraphStruct.DgraphTask) string {
	name := strings.TrimSpace(t.Name)
	if name == "" {
		name = "(untitled task)"
	}
	status := t.Status
	if t.CustomStatusName != nil && *t.CustomStatusName != "" {
		status = *t.CustomStatusName
	}
	parts := []string{}
	if status != "" {
		parts = append(parts, "status: "+status)
	}
	owner := "no owner"
	if t.Assignee != nil {
		if n := strings.TrimSpace(t.Assignee.UserFullName); n != "" {
			owner = n
		} else if n := strings.TrimSpace(t.Assignee.UserName); n != "" {
			owner = n
		}
	}
	parts = append(parts, "owner: "+owner)
	if t.DueDate != nil && t.DueDate.Year() > 1970 {
		parts = append(parts, "due: "+t.DueDate.Format("Mon 2 Jan 2006"))
	}
	if t.Priority != "" {
		parts = append(parts, "priority: "+t.Priority)
	}
	return "- " + name + " (" + strings.Join(parts, ", ") + ")"
}
