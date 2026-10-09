package business

// Task agent read tool. The write tools (create/update_status/assign/due_date)
// all need a task_uuid the model can only get from RAG context. list_tasks
// closes the loop: the agent can find the user's task by name/status/overdue,
// learn its uuid, and then chain into an update - all under the same read-then-
// act flow the assistant already uses for "summarize then DM".
//
// Read-only and scoped to the acting user's OWN assigned tasks, so it can never
// surface work the user could not already see in "My Tasks".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	ai "github.com/akashc777/OneCamp/services/AI"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
)

// listTasksMaxResults bounds how many tasks we feed back to the model so a
// user with hundreds of tasks can't blow the context budget.
const listTasksMaxResults = 25

// buildTaskFilterQuery turns the optional status / overdue / search params
// into a safe DQL filter string shared by the user-task and project-task
// listings. Status is resolved as a person would say it (see QueryClause); the search term is
// sanitised and regex-escaped before interpolation.
func buildTaskFilterQuery(ctx context.Context, action ai.ProposedAction) (string, error) {
	var filters []string

	if status := strings.TrimSpace(action.Params["status"]); status != "" {
		// A built-in status, or a project's own by name: in the project when
		// listing one, else in whichever of the person's projects has it.
		clause, err := taskStatusBusiness.QueryClause(ctx, strings.TrimSpace(action.Params["project_uuid"]), status)
		if errors.Is(err, taskStatusBusiness.ErrUnknownStatus) {
			return "", fmt.Errorf("unknown status %q. %s", status, taskStatusBusiness.Describe(ctx, strings.TrimSpace(action.Params["project_uuid"])))
		}
		if err != nil {
			return "", err
		}
		filters = append(filters, clause)
	}

	if strings.EqualFold(strings.TrimSpace(action.Params["filter"]), "overdue") {
		now := time.Now().Format(time.RFC3339Nano)
		filters = append(filters, fmt.Sprintf(dgraphStruct.TASK_OPEN_FILTER+` AND lt(task_due_date, "%s") AND gt(task_due_date, "1970-01-01T00:00:00Z")`, now))
	}

	if search := strings.TrimSpace(action.Params["search"]); search != "" {
		// Sanitise + regex-escape before DQL interpolation. An empty result
		// after sanitisation just means "no name filter".
		if safe, sErr := dgraphquery.SanitizeSearchTerm(search); sErr == nil {
			filters = append(filters, fmt.Sprintf(`regexp(task_name, /.*%s.*/i)`, safe))
		}
	}

	return strings.Join(filters, " AND "), nil
}

// formatTaskLine renders one task as a single bullet for a tool result. When
// withAssignee is true (project listings, where tasks belong to others) the
// assignee name is included. The task_uuid is appended for the model to chain
// into an update tool; the final user-facing answer has UUIDs stripped by
// SanitizeResponse.
func formatTaskLine(t *dgraphStruct.DgraphTask, withAssignee bool) string {
	name := strings.TrimSpace(t.Name)
	if name == "" {
		name = "(untitled task)"
	}
	status := t.Status
	if status == "" {
		status = "unknown"
	}
	// A project's own status reads by its name, with the stage it counts as,
	// so the model can both say it and reason about it ("QA (inReview)").
	if t.CustomStatusName != nil && *t.CustomStatusName != "" {
		status = fmt.Sprintf("%s (%s)", *t.CustomStatusName, status)
	}
	line := fmt.Sprintf("- %s [status: %s", name, status)
	if t.Priority != "" {
		line += fmt.Sprintf(", priority: %s", t.Priority)
	}
	if t.DueDate != nil && t.DueDate.Year() > 1970 {
		line += fmt.Sprintf(", due: %s", t.DueDate.Format("Jan 2 2006"))
	}
	if withAssignee {
		assignee := "unassigned"
		if t.Assignee != nil && strings.TrimSpace(t.Assignee.UserName) != "" {
			assignee = t.Assignee.UserName
		}
		line += fmt.Sprintf(", assignee: %s", assignee)
	}
	if t.Project != nil && strings.TrimSpace(t.Project.Name) != "" {
		line += fmt.Sprintf(", project: %s", t.Project.Name)
	}
	line += fmt.Sprintf("] (task_uuid: %s)", t.Uuid)
	return line
}

// executeListTasks lists the acting user's assigned tasks, optionally filtered
// by status, overdue, and a name search. Read-only.
func executeListTasks(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	// Read tools only need the dgraph user (uuid + uid), so we skip the extra
	// postgres lookup that getUserInfoForExecutor would do.
	actingUser, err := getDgraphUserForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}

	filterQuery, err := buildTaskFilterQuery(ctx, action)
	if err != nil {
		return "", nil, err
	}

	dgraphUser, err := userBusiness.GetDgraphUserTaskList(
		ctx,
		actingUser.Uuid,
		actingUser.Uid,
		filterQuery,
		"orderasc: task_due_date",
		listTasksMaxResults,
		0,
		false,
	)
	if err != nil {
		return "", nil, fmt.Errorf("failed to list tasks: %w", err)
	}
	if dgraphUser == nil || len(dgraphUser.Tasks) == 0 {
		return "You have no tasks matching that.", nil, nil
	}
	// The sponsor's tasks, and when someone else asked, only those in projects
	// that person is in too: project membership is what grants sight of a task.
	asker, err := askerView(ctx)
	if err != nil {
		return "", nil, err
	}
	if asker != nil {
		kept := dgraphUser.Tasks[:0:0]
		for _, t := range dgraphUser.Tasks {
			if t != nil && t.Project != nil && asker.projects[t.Project.Uuid] {
				kept = append(kept, t)
			}
		}
		// The total counted every task, so it would announce ones that are not
		// shown; the narrowed list is the whole answer.
		dgraphUser.Tasks, dgraphUser.TaskCount = kept, uint64(len(kept))
		if len(kept) == 0 {
			return "There are no tasks matching that which you can see.", nil, nil
		}
	}

	meta := taskListMeta("Your tasks", dgraphUser.Tasks, int(dgraphUser.TaskCount))
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Your tasks (%d shown):\n", len(dgraphUser.Tasks)))
	for _, t := range dgraphUser.Tasks {
		b.WriteString(formatTaskLine(t, false))
		b.WriteString("\n")
	}
	if dgraphUser.TaskCount > uint64(len(dgraphUser.Tasks)) {
		b.WriteString(fmt.Sprintf("\n(Showing %d of %d. Narrow it down with a status or a search term.)", len(dgraphUser.Tasks), dgraphUser.TaskCount))
	}

	return strings.TrimSpace(b.String()), meta, nil
}

// TaskRow is one task as a screen shows it: the structured twin of formatTaskLine.
type TaskRow struct {
	UUID     string `json:"uuid"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
	Due      string `json:"due,omitempty"`
	Overdue  bool   `json:"overdue,omitempty"`
	Project  string `json:"project,omitempty"`
	Assignee string `json:"assignee,omitempty"`
	URL      string `json:"url"`
}

// TaskList is a list of tasks with what the view needs to title and link it.
type TaskList struct {
	Title string    `json:"title"`
	Total int       `json:"total"`
	Tasks []TaskRow `json:"tasks"`
}

// taskListMeta builds the MetaStructuredJSON for a task list. Never fails a
// tool call: a marshal error drops only the structured copy.
func taskListMeta(title string, tasks []*dgraphStruct.DgraphTask, total int) map[string]string {
	b, err := json.Marshal(taskList(title, tasks, total, authService.FrontendBaseURL(), time.Now()))
	if err != nil {
		return nil
	}
	return map[string]string{ai.MetaStructuredJSON: string(b)}
}

// taskList is taskListMeta's data, pure for its test.
func taskList(title string, tasks []*dgraphStruct.DgraphTask, total int, base string, now time.Time) TaskList {
	out := TaskList{Title: title, Total: total, Tasks: make([]TaskRow, 0, len(tasks))}
	for _, t := range tasks {
		if t == nil {
			continue
		}
		row := TaskRow{UUID: t.Uuid, Name: strings.TrimSpace(t.Name), Status: t.Status, Priority: t.Priority, URL: base + "/app/task/" + t.Uuid}
		if row.Name == "" {
			row.Name = "(untitled task)"
		}
		if t.CustomStatusName != nil && *t.CustomStatusName != "" {
			row.Status = *t.CustomStatusName
		}
		if t.DueDate != nil && t.DueDate.Year() > 1970 {
			row.Due = t.DueDate.UTC().Format(time.RFC3339)
			row.Overdue = t.DueDate.Before(now) && t.Status != dgraphStruct.TASK_STATUS_DONE && t.Status != dgraphStruct.TASK_STATUS_CANCELED
		}
		if t.Project != nil {
			row.Project = strings.TrimSpace(t.Project.Name)
		}
		if t.Assignee != nil {
			row.Assignee = strings.TrimSpace(t.Assignee.UserName)
		}
		out.Tasks = append(out.Tasks, row)
	}
	if out.Total < len(out.Tasks) {
		out.Total = len(out.Tasks)
	}
	return out
}

// executeListProjectTasks lists the tasks in a project the user can see. It
// enforces the same rule as controllers/Project/GetProjectTaskList: the user
// must be a MEMBER of the project (not necessarily an admin) to read its
// tasks. Read-only.
func executeListProjectTasks(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	projectUUID := strings.TrimSpace(action.Params["project_uuid"])
	if projectUUID == "" {
		return "", nil, fmt.Errorf("project_uuid is required")
	}
	if _, err := uuid.Parse(projectUUID); err != nil {
		return "", nil, fmt.Errorf("invalid project UUID")
	}

	actingUser, err := getDgraphUserForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}

	filterQuery, err := buildTaskFilterQuery(ctx, action)
	if err != nil {
		return "", nil, err
	}

	// Single query: GetDgraphProjectTaskList returns the project name, the
	// caller's membership flag, and the tasks. We gate on the returned
	// IsProjectMember (the same rule controllers/Project/GetProjectTaskList
	// enforces) without a separate project-info round-trip.
	taskProject, err := projectBusiness.GetDgraphProjectTaskList(
		ctx,
		projectUUID,
		actingUser.Uid,
		filterQuery,
		"orderasc: task_due_date",
		listTasksMaxResults,
		0,
		false,
	)
	if err != nil {
		return "", nil, fmt.Errorf("failed to list project tasks: %w", err)
	}
	if taskProject == nil || taskProject.Uuid == "" {
		return "", nil, fmt.Errorf("project not found or you don't have access")
	}
	if taskProject.IsProjectMember == 0 {
		return "", nil, fmt.Errorf("you must be a member of this project to view its tasks")
	}

	projectName := strings.TrimSpace(taskProject.Name)
	if projectName == "" {
		projectName = "this project"
	}
	if len(taskProject.Tasks) == 0 {
		return fmt.Sprintf("No tasks in %s match that.", projectName), nil, nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Tasks in %s (%d shown):\n", projectName, len(taskProject.Tasks)))
	for _, t := range taskProject.Tasks {
		b.WriteString(formatTaskLine(t, true))
		b.WriteString("\n")
	}
	if taskProject.TaskCount > uint64(len(taskProject.Tasks)) {
		b.WriteString(fmt.Sprintf("\n(Showing %d of %d. Narrow it down with a status or a search term.)", len(taskProject.Tasks), taskProject.TaskCount))
	}

	return strings.TrimSpace(b.String()), taskListMeta("Tasks in "+projectName, taskProject.Tasks, int(taskProject.TaskCount)), nil
}
