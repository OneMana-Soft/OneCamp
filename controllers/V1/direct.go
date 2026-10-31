package controllers

// The public API's tasks, projects, messages and search, on the business
// functions the app's own screens call, with the same checks.
//
// These ran through the AI assistant's tool executors, which the AI-free
// edition doesn't have: there every one answered 501 "AI features are not
// supported on this build", though tokens with their scopes could be made,
// so "post to #deploys from CI" failed on every AI-free install (SA-BUG-11).
// Both editions run this same code now. A message sent through the API is its
// token owner's, no longer marked as written by AI. The answer keeps the
// SDK's shape: {"message": what happened, in words; "data": ids and names}.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	taskAdapter "github.com/akashc777/OneCamp/adapter/Task"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	appBusiness "github.com/akashc777/OneCamp/business/App"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	taskPriority "github.com/akashc777/OneCamp/business/TaskPriority"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// structuredKey names the JSON copy of a list in data, as the AI tools named
// it, so a caller reading it before reads it still.
const structuredKey = "structured_json"

// apiTaskMax is the most tasks one list answers with.
const apiTaskMax = 50

func apiUser(r *http.Request) userModels.UserInfo {
	u, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return u
}

func answer(w http.ResponseWriter, status int, message string, data map[string]string) {
	helpers.WriteJSON(w, status, helpers.Envolope{"message": message, "data": data})
}

func refuse(w http.ResponseWriter, status int, msg string) {
	helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
}

// refuseSend answers a send the rules refused as the app would, and anything
// else as a fault.
func refuseSend(w http.ResponseWriter, err error) {
	if rj, ok := sendBusiness.AsRejection(err); ok {
		refuse(w, rj.Status, rj.Msg)
		return
	}
	refuse(w, http.StatusInternalServerError, "couldn't send the message")
}

// audited records a write made through the public API (NFR2 / req 5.2).
// Reads aren't recorded: many, and they change nothing.
func audited(r *http.Request, op string, extra map[string]interface{}) {
	meta := map[string]interface{}{"token_id": helpers.GetApiTokenID(r.Context())}
	for k, v := range extra {
		meta[k] = v
	}
	auditBusiness.Record(r, "api."+op, auditBusiness.CategoryIntegration, "Public API call: "+op, meta)
}

func asJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// ── tasks ─────────────────────────────────────────────────────────────────────

// apiTask is one task as a list answers it (the AI tools' TaskRow).
type apiTask struct {
	UUID     string `json:"uuid"`
	Key      string `json:"key,omitempty"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
	Due      string `json:"due,omitempty"`
	Overdue  bool   `json:"overdue,omitempty"`
	Project  string `json:"project,omitempty"`
	Assignee string `json:"assignee,omitempty"`
	URL      string `json:"url"`
}

type apiTaskList struct {
	Title string    `json:"title"`
	Total int       `json:"total"`
	Tasks []apiTask `json:"tasks"`
}

// taskFilter is the graph filter for a task list: a status (built-in, or a
// project's own by name), overdue, and words in the name.
func taskFilter(ctx context.Context, status, filter, search string) (string, error) {
	var parts []string
	if status = strings.TrimSpace(status); status != "" {
		clause, err := taskStatusBusiness.QueryClause(ctx, "", status)
		if errors.Is(err, taskStatusBusiness.ErrUnknownStatus) {
			return "", fmt.Errorf("unknown status %q. %s", status, taskStatusBusiness.Describe(ctx, ""))
		}
		if err != nil {
			return "", err
		}
		parts = append(parts, clause)
	}
	if strings.EqualFold(strings.TrimSpace(filter), "overdue") {
		now := time.Now().Format(time.RFC3339Nano)
		parts = append(parts, fmt.Sprintf(dgraphStruct.TASK_OPEN_FILTER+` AND lt(task_due_date, "%s") AND gt(task_due_date, "1970-01-01T00:00:00Z")`, now))
	}
	if search = strings.TrimSpace(search); search != "" {
		if safe, err := dgraphquery.SanitizeSearchTerm(search); err == nil {
			parts = append(parts, fmt.Sprintf(`regexp(task_name, /.*%s.*/i)`, safe))
		}
	}
	return strings.Join(parts, " AND "), nil
}

func taskRows(tasks []*dgraphStruct.DgraphTask, total int, now time.Time) apiTaskList {
	base := authService.FrontendBaseURL()
	out := apiTaskList{Title: "Your tasks", Total: total, Tasks: make([]apiTask, 0, len(tasks))}
	for _, t := range tasks {
		if t == nil {
			continue
		}
		row := apiTask{UUID: t.Uuid, Key: t.Key, Name: strings.TrimSpace(t.Name), Status: t.Status, Priority: t.Priority, URL: base + "/app/task/" + t.Uuid}
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

// taskPlaces are the places a task sits in, for an app: the task, and its project.
func taskPlaces(t *dgraphStruct.DgraphTask) []appBusiness.Place {
	places := []appBusiness.Place{{Kind: "task", ID: t.Uuid}}
	if t.Project != nil {
		places = append(places, appBusiness.Place{Kind: "project", ID: t.Project.Uuid})
	}
	return places
}

// ListTasksV1 GET /v1/tasks — scope tasks:read. The token owner's tasks; an
// app's, only those in its places, counted alone.
// Optional query params: status, filter (overdue), search.
func ListTasksV1(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := apiUser(r)
	q := r.URL.Query()
	filter, err := taskFilter(ctx, q.Get("status"), q.Get("filter"), q.Get("search"))
	if err != nil {
		refuse(w, http.StatusBadRequest, err.Error())
		return
	}
	got, err := userBusiness.GetDgraphUserTaskList(ctx, user.UserDgraphInfo.Uuid, user.UserDgraphInfo.Uid, filter, "orderasc: task_due_date", apiTaskMax, 0, false)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "couldn't list the tasks")
		return
	}
	var tasks []*dgraphStruct.DgraphTask
	total := 0
	if got != nil {
		tasks, total = got.Tasks, int(got.TaskCount)
	}
	tasks, app := narrowed(r, tasks, taskPlaces)
	if app != nil {
		total = len(tasks)
	}
	if len(tasks) == 0 {
		answer(w, http.StatusOK, "You have no tasks matching that.", nil)
		return
	}
	list := taskRows(tasks, total, time.Now())
	var b strings.Builder
	fmt.Fprintf(&b, "Your tasks (%d shown):", len(list.Tasks))
	for _, t := range list.Tasks {
		if t.Key != "" {
			fmt.Fprintf(&b, "\n- %s %s [%s", t.Key, t.Name, t.Status)
		} else {
			fmt.Fprintf(&b, "\n- %s [%s", t.Name, t.Status)
		}
		if t.Due != "" {
			fmt.Fprintf(&b, ", due %s", t.Due[:10])
		}
		if t.Project != "" {
			fmt.Fprintf(&b, ", project: %s", t.Project)
		}
		fmt.Fprintf(&b, "] (task_uuid: %s)", t.UUID)
	}
	answer(w, http.StatusOK, b.String(), map[string]string{structuredKey: asJSON(list)})
}

// CreateTaskV1 POST /v1/tasks — scope tasks:write. Body: task_name,
// project_uuid, description, priority, assignee_uuid. Whoever may make tasks
// in the project in the app may (Work Access).
func CreateTaskV1(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := apiUser(r)
	params, err := decodeParams(r)
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(params["task_name"])
	if name == "" {
		refuse(w, http.StatusBadRequest, "task_name is required")
		return
	}
	projectID, err := uuid.Parse(strings.TrimSpace(params["project_uuid"]))
	if err != nil {
		refuse(w, http.StatusBadRequest, "project_uuid must be a project's id")
		return
	}
	project, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, projectID.String(), user.UserDgraphInfo.Uid)
	if err != nil || project == nil || project.Uuid == "" {
		refuse(w, http.StatusNotFound, "project not found or you don't have access")
		return
	}
	if !project.ReaderIsIn() {
		refuse(w, http.StatusNotFound, "project not found or you don't have access")
		return
	}
	// As in the app: members and admins make tasks, by the project's "Who
	// can change tasks" (Work Access).
	standing, err := workAccess.Of(ctx, user.UserDgraphInfo.Uuid, project)
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, "couldn't check what you can do in that project; try again")
		return
	}
	if err := standing.Check(workAccess.NeedCreateTask); err != nil {
		refuse(w, http.StatusForbidden, err.Error())
		return
	}
	priority := strings.ToLower(strings.TrimSpace(params["priority"]))
	if priority == "" {
		priority = dgraphStruct.TASK_PRIORITY_MEDIUM
	}
	if !taskPriority.Is(priority) {
		refuse(w, http.StatusBadRequest, "priority is urgent, high, medium or low")
		return
	}
	var assignee *dgraphStruct.DgraphUser
	if a := strings.TrimSpace(params["assignee_uuid"]); a != "" {
		if _, err := uuid.Parse(a); err != nil {
			refuse(w, http.StatusBadRequest, "assignee_uuid must be a person's id")
			return
		}
		if a == user.UserDgraphInfo.Uuid {
			assignee = &user.UserDgraphInfo
		} else if assignee, err = userBusiness.GetDgraphUserInfoByUUID(ctx, a); err != nil || assignee == nil {
			refuse(w, http.StatusBadRequest, "couldn't find the person to assign it to")
			return
		}
	}
	input := taskAdapter.CreateOrUpdateTaskInput{
		TaskName:        name,
		TaskDescription: helpers.PlainTextToHTML(params["description"]),
		Priority:        priority,
		Status:          dgraphStruct.TASK_STATUS_TODO,
		ProjectUuid:     projectID.String(),
	}
	if assignee != nil {
		input.AssigneeUuid = assignee.Uuid
	}
	taskID, err := taskBusiness.CreateTask(ctx, projectID, &user, project, assignee, input, nil)
	if errors.Is(err, helpers.ErrConflict) {
		// The app's answer: an assignee who can't be given work, by name.
		refuse(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		refuse(w, http.StatusInternalServerError, "couldn't create the task")
		return
	}
	audited(r, "create_task", map[string]interface{}{"task_id": taskID.String(), "project_id": projectID.String()})
	key := taskKeys.KeyOf(ctx, taskID)
	msg := fmt.Sprintf("Task %s created in project %s (priority: %s)", taskKeys.Named(key, strconv.Quote(name)), project.Name, priority)
	if assignee != nil {
		msg += ", assigned to " + assignee.DisplayName()
	}
	answer(w, http.StatusOK, msg, map[string]string{
		"tool": "create_task", "task_uuid": taskID.String(), "key": key, "project_uuid": projectID.String(), "task_name": name,
	})
}

// UpdateTaskStatusV1 POST /v1/tasks/{id}/status — scope tasks:write. {id} is
// the task's id or its key (Q4-12). Body: status (built-in, or one of the
// project's own by name).
func UpdateTaskStatusV1(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := apiUser(r)
	params, err := decodeParams(r)
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid request body")
		return
	}
	status := strings.TrimSpace(params["status"])
	if status == "" {
		refuse(w, http.StatusBadRequest, "status is required")
		return
	}
	// The task's id, or its key ("Q4-12").
	taskID, err := taskKeys.Ref(ctx, chi.URLParam(r, "id"))
	if errors.Is(err, taskKeys.ErrNoTask) {
		refuse(w, http.StatusNotFound, "task not found or you don't have access")
		return
	}
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, "couldn't look the task up; try again")
		return
	}
	task, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskID.String(), user.UserDgraphInfo.Uid)
	if err != nil || task == nil || task.Uuid == "" {
		refuse(w, http.StatusNotFound, "task not found or you don't have access")
		return
	}
	if !taskBusiness.CanViewTask(task, user.UserDgraphInfo.Uid) {
		refuse(w, http.StatusNotFound, "task not found or you don't have access")
		return
	}
	standing, facts, err := workAccess.OfTask(ctx, user.UserDgraphInfo.Uuid, user.UserDgraphInfo.Uid, task)
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, "couldn't check what you can do with that task; try again")
		return
	}
	if err := standing.CheckTask(workAccess.NeedEditTask, facts); err != nil {
		refuse(w, http.StatusForbidden, err.Error())
		return
	}
	err = taskBusiness.UpdateTaskStatusByTaskUUID(ctx, taskID, status, task, &user.UserDgraphInfo)
	if r, held := taskBusiness.RefusedStatus(err); held {
		refuse(w, http.StatusConflict, r.Why)
		return
	}
	switch {
	case errors.Is(err, taskBusiness.ErrTaskDeleted):
		refuse(w, http.StatusNotFound, "that task was deleted")
		return
	case errors.Is(err, taskStatusBusiness.ErrUnknownStatus):
		refuse(w, http.StatusBadRequest, fmt.Sprintf("invalid status %q (use one of: %s)", status, taskStatusBusiness.Describe(ctx, task.Project.Uuid)))
		return
	case err != nil:
		refuse(w, http.StatusInternalServerError, "couldn't change the status")
		return
	}
	audited(r, "update_task_status", map[string]interface{}{"task_id": taskID.String()})
	name := task.Name
	if name == "" {
		name = taskID.String()
	}
	key := taskKeys.KeyOf(ctx, taskID)
	answer(w, http.StatusOK, fmt.Sprintf("Moved task %s to %s", taskKeys.Named(key, strconv.Quote(name)), status), map[string]string{
		"tool": "update_task_status", "task_uuid": taskID.String(), "key": key, "status": status,
	})
}

// ── projects ──────────────────────────────────────────────────────────────────

type apiProject struct {
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	CanManage bool   `json:"can_manage_tasks"`
	Team      string `json:"team,omitempty"`
}

// ListProjectsV1 GET /v1/projects — scope projects:read. The projects the
// token owner is in (an app's, only those it was added to), and whether they
// can create and manage tasks in each.
func ListProjectsV1(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := apiUser(r)
	got, err := userBusiness.GetDgraphUserProjectList(ctx, user.UserDgraphInfo.Uuid)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "couldn't list the projects")
		return
	}
	if got != nil {
		got.Projects, _ = narrowed(r, got.Projects, func(p *dgraphStruct.DgraphProject) []appBusiness.Place {
			return []appBusiness.Place{{Kind: "project", ID: p.Uuid}}
		})
	}
	admin := map[string]bool{}
	if ps, err := projectBusiness.GetDgraphProjectListByAdminDgraphUID(ctx, user.UserDgraphInfo.Uid); err == nil {
		for _, p := range ps {
			if p != nil {
				admin[p.Uuid] = true
			}
		}
	}
	// Whether they make and change tasks in each, by Work Access's rule: one
	// read of their roles for the lot. The list is of projects they're in.
	var standings map[string]workAccess.Standing
	if got != nil {
		in := make([]*dgraphStruct.DgraphProject, 0, len(got.Projects))
		for _, p := range got.Projects {
			if p != nil && p.Uuid != "" {
				read := &dgraphStruct.DgraphProject{Uuid: p.Uuid, IsProjectMember: 1}
				if admin[p.Uuid] {
					read.IsProjectAdmin = 1
				}
				in = append(in, read)
			}
		}
		if standings, err = workAccess.OfMany(ctx, user.UserDgraphInfo.Uuid, in); err != nil {
			refuse(w, http.StatusServiceUnavailable, "couldn't list the projects; try again")
			return
		}
	}
	var list []apiProject
	if got != nil {
		for _, p := range got.Projects {
			if p == nil || p.Uuid == "" || helpers.IsSoftDeleted(p.DeletedAt) {
				continue
			}
			row := apiProject{UUID: p.Uuid, Name: strings.TrimSpace(p.Name), CanManage: standings[strings.ToLower(p.Uuid)].Can(workAccess.NeedEditTask)}
			if row.Name == "" {
				row.Name = "(untitled project)"
			}
			if p.Team != nil {
				row.Team = strings.TrimSpace(p.Team.Name)
			}
			list = append(list, row)
		}
	}
	if len(list) == 0 {
		answer(w, http.StatusOK, "You are not a member of any projects.", nil)
		return
	}
	var b strings.Builder
	b.WriteString("Your projects:")
	for _, p := range list {
		manage := "no"
		if p.CanManage {
			manage = "yes"
		}
		fmt.Fprintf(&b, "\n- %s [can create/manage tasks: %s", p.Name, manage)
		if p.Team != "" {
			fmt.Fprintf(&b, ", team: %s", p.Team)
		}
		fmt.Fprintf(&b, "] (project_uuid: %s)", p.UUID)
	}
	answer(w, http.StatusOK, b.String(), map[string]string{structuredKey: asJSON(list)})
}

// ── messages ──────────────────────────────────────────────────────────────────

// SendMessageV1 POST /v1/messages/channel — scope messages:write. Body:
// channel_uuid, text (plain text: paragraphs, line breaks and lists). Under
// the rules the composer's messages meet (business/Send).
func SendMessageV1(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := apiUser(r)
	params, err := decodeParams(r)
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid request body")
		return
	}
	channelID := strings.TrimSpace(params["channel_uuid"])
	html := helpers.PlainTextToHTML(params["text"])
	if html == "" {
		refuse(w, http.StatusBadRequest, "text is required")
		return
	}
	in := &postAdapter.InputCreateOrUpdatePostInfo{HTMLText: html, ChannelUuid: channelID}
	post, err := sendBusiness.PrepareChannelPost(ctx, &user, in)
	if err != nil {
		refuseSend(w, err)
		return
	}
	created, err := post.Commit(ctx)
	if err != nil || created == nil {
		refuseSend(w, err)
		return
	}
	audited(r, "send_message", map[string]interface{}{"channel_id": channelID, "post_id": created.Uuid})
	answer(w, http.StatusOK, "Message sent to the channel.", map[string]string{
		"tool": "send_message", "channel_uuid": channelID, "post_uuid": created.Uuid,
		"post_text": html, "post_created_at": created.PostCreated.Format(time.RFC3339),
	})
}

// SendDMV1 POST /v1/messages/dm — scope messages:write. Body: to_uuid, text.
func SendDMV1(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := apiUser(r)
	params, err := decodeParams(r)
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid request body")
		return
	}
	to := strings.TrimSpace(params["to_uuid"])
	if _, err := uuid.Parse(to); err != nil {
		refuse(w, http.StatusBadRequest, "to_uuid must be a person's id")
		return
	}
	html := helpers.PlainTextToHTML(params["text"])
	if html == "" {
		refuse(w, http.StatusBadRequest, "text is required")
		return
	}
	dm, err := sendBusiness.PrepareDirectMessage(ctx, &user, &chatAdapter.ChatInfo{TextHtml: html, ToUuid: to})
	if err != nil {
		refuseSend(w, err)
		return
	}
	created, err := dm.Commit(ctx)
	if err != nil || created == nil {
		refuseSend(w, err)
		return
	}
	audited(r, "send_dm", map[string]interface{}{"to": to, "chat_id": created.Uuid})
	answer(w, http.StatusOK, "DM sent.", map[string]string{
		"tool": "send_dm", "dm_grouping_id": helpers.GetGroupingId(user.UserDgraphInfo.Uuid, to),
		"chat_uuid": created.Uuid, "chat_text": html, "chat_created_at": created.ChatCreated.Format(time.RFC3339),
		"recipient_uuid": to,
	})
}

// ── search ────────────────────────────────────────────────────────────────────

// SearchV1 GET/POST /v1/search — scope search:read. The token owner's search
// of the workspace, the one the app's search box runs. Query via ?q= or a
// JSON body {"query": "..."}.
func SearchV1(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := apiUser(r)
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		params, err := decodeParams(r)
		if err != nil {
			refuse(w, http.StatusBadRequest, "invalid request body")
			return
		}
		query = strings.TrimSpace(params["query"])
	}
	if query == "" {
		refuse(w, http.StatusBadRequest, "query is required")
		return
	}
	page, err := globalSearchBusiness.GetUnifiedGlobalSearch(ctx, accessBusiness.SubjectFor(ctx, &user), user.UserPostgresInfo.EmailID,
		user.UserDgraphInfo.Channels, user.UserDgraphInfo.Projects, user.UserDgraphInfo.Teams, query)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "couldn't search")
		return
	}
	page.Page, _ = narrowed(r, page.Page, hitPlaces)
	answer(w, http.StatusOK, fmt.Sprintf("%d results for %q.", len(page.Page), query), map[string]string{structuredKey: asJSON(page)})
}

// hitPlaces are the places a search hit sits in, for an app: a message, a
// comment or a file in its channel or project (or a comment on its doc), a
// task in itself or its project, a doc, a project, a channel. A DM, a group,
// a board, a team or a person is no place an app is added to, so an app finds
// none of them.
func hitPlaces(h *openSearchStruct.GlobalSearchOpenSearchResp) []appBusiness.Place {
	switch {
	case h == nil:
		return nil
	case h.Post != nil:
		return []appBusiness.Place{{Kind: "channel", ID: h.Post.PostChannelUuid}}
	case h.Comment != nil:
		return []appBusiness.Place{{Kind: "channel", ID: h.Comment.CommentChannelUuid}, {Kind: "project", ID: h.Comment.CommentProjectUuid}, {Kind: "doc", ID: h.Comment.CommentDocUuid}}
	case h.Attachment != nil:
		return []appBusiness.Place{{Kind: "channel", ID: h.Attachment.AttachmentChannelUuid}, {Kind: "project", ID: h.Attachment.AttachmentProjectUuid}}
	case h.Task != nil:
		return []appBusiness.Place{{Kind: "task", ID: h.Task.Uuid}, {Kind: "project", ID: h.Task.TaskProjectUuid}}
	case h.Doc != nil:
		return []appBusiness.Place{{Kind: "doc", ID: h.Doc.Uuid}}
	case h.Project != nil:
		return []appBusiness.Place{{Kind: "project", ID: h.Project.Uuid}}
	case h.Channel != nil:
		return []appBusiness.Place{{Kind: "channel", ID: h.Channel.Uuid}}
	}
	return nil
}
