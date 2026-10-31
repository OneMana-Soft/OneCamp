package Domain

// The tasks in a project that have an assignee, as an upgrade's repair of
// old assignments reads them (business/AssigneeRepair): a page at a time in
// the graph's uid order, each with whether its assignee is one of its
// project's people.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// AssignedTask is a task in a project with an assignee, read with its
// deletion time, its assignee (their names, and whether they're a bot or
// someone who can't sign in) and its project. InProject is whether the
// assignee is on the project's member or admin edges, active or not, as
// TaskReader.MayRead reads them.
type AssignedTask struct {
	Task      *dgraphStruct.DgraphTask
	InProject bool
}

// AssignedInProjectsPage reads up to size tasks in a project with an
// assignee, after the task with graph uid after ("" for the first page).
// next is the uid the page after it starts after, "" after the last. Two
// reads: the page, then its projects' edges to its assignees alone, so a big
// project's people are never read whole.
func AssignedInProjectsPage(ctx context.Context, after string, size int) (page []AssignedTask, next string, err error) {
	from := ""
	if after != "" {
		if !helpers.IsDgraphUID(after) {
			return nil, "", fmt.Errorf("assigned tasks: page after %q, not a uid", after)
		}
		from = ", after: " + after
	}
	tasks, err := dgexec.List[*dgraphStruct.DgraphTask](ctx, `{
		page(func: has(task_assignee), first: `+strconv.Itoa(size)+from+`) @filter(has(task_project)) {
			uid
			task_uuid
			task_deleted_at
			task_assignee { uid user_uuid user_name user_full_name user_email_id is_bot is_external }
			task_project { uid project_uuid }
		}
	}`, nil, "page")
	if err != nil {
		return nil, "", err
	}
	if len(tasks) == size {
		next = tasks[size-1].Uid
	}
	var projects, people []string
	seen := map[string]bool{}
	for _, t := range tasks {
		if !placed(t) {
			continue
		}
		if !seen[t.Project.Uid] {
			seen[t.Project.Uid], projects = true, append(projects, t.Project.Uid)
		}
		if !seen[t.Assignee.Uid] {
			seen[t.Assignee.Uid], people = true, append(people, t.Assignee.Uid)
		}
	}
	in := map[string]bool{}
	if len(projects) > 0 {
		type node struct {
			Uid string `json:"uid"`
		}
		them := strings.Join(people, ", ")
		rows, err := dgexec.List[struct {
			Uid     string `json:"uid"`
			Members []node `json:"project_members"`
			Admins  []node `json:"project_admins"`
		}](ctx, `{
			p(func: uid(`+strings.Join(projects, ", ")+`)) {
				uid
				project_members @filter(uid(`+them+`)) { uid }
				project_admins @filter(uid(`+them+`)) { uid }
			}
		}`, nil, "p")
		if err != nil {
			return nil, "", err
		}
		for _, r := range rows {
			for _, m := range append(r.Members, r.Admins...) {
				in[r.Uid+" "+m.Uid] = true
			}
		}
	}
	page = make([]AssignedTask, 0, len(tasks))
	for _, t := range tasks {
		if placed(t) {
			page = append(page, AssignedTask{Task: t, InProject: in[t.Project.Uid+" "+t.Assignee.Uid]})
		}
	}
	return page, next, nil
}

// placed is a task read with its project and an assignee who is someone:
// an edge left to a node with no user id names nobody.
func placed(t *dgraphStruct.DgraphTask) bool {
	return t != nil && t.Uuid != "" && t.Project != nil && helpers.IsDgraphUID(t.Project.Uid) &&
		t.Assignee != nil && t.Assignee.Uuid != "" && helpers.IsDgraphUID(t.Assignee.Uid)
}
