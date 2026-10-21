package business

import (
	"context"
	"fmt"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// NewTask is one task CreateTasks makes: what a person would type, and
// optionally the task it sits under, by its place in the same batch (Parent,
// -1 for none, which must come before it).
type NewTask struct {
	Input  adapter.CreateOrUpdateTaskInput
	Parent int
}

// CreateTasks makes many tasks in a project at once (the BE audit's #30: a
// whiteboard's notes, a project started from a template), as CreateTask and
// CreateSubTask make one, unassigned and mentioning no one: their rows and
// numbers in one statement, the graph in one write, search in one _bulk, the
// project's people told once, and one task.created event each. It answers
// their ids in the order given. A bad date or status refuses the batch before
// anything is written.
func CreateTasks(ctx context.Context, projectUUID uuid.UUID, user *model.UserInfo, project *dgraphStruct.DgraphProject, tasks []NewTask) ([]uuid.UUID, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	if project == nil || project.Team == nil {
		return nil, fmt.Errorf("business/CreateTasks: project %s has no team", projectUUID)
	}
	// A copy of a restricted task is kept to that one's people one by one.
	if workAccess.CopiesRestricted(ctx) {
		return createOneByOne(ctx, projectUUID, user, project, tasks)
	}
	now := time.Now()
	nodes, search, err := newTaskNodes(ctx, projectUUID, user, project, tasks, now)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(nodes))
	for i, n := range nodes {
		ids[i], _ = uuid.Parse(n.Uuid)
	}
	numbers, err := domain.CreateTasks(ctx, ids, projectUUID, user.UserPostgresInfo.Id, now)
	if err != nil {
		return nil, err
	}
	// A project with no key yet numbers them in its sweep, once.
	unnumbered := false
	for _, n := range numbers {
		unnumbered = unnumbered || n == 0
	}
	if unnumbered {
		settleKey(ctx, projectUUID, 0)
	}
	// The project holds them all, in one node of the same write.
	holds := make([]*dgraphStruct.DgraphTask, len(nodes))
	for i, n := range nodes {
		holds[i] = &dgraphStruct.DgraphTask{Uid: n.Uid}
	}
	if _, err := domain.CreateDgraphTasks(ctx, nodes, &dgraphStruct.DgraphProject{Uid: project.Uid, Tasks: holds}); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CreateTasks failed to write %d tasks to the graph err: %+v", len(nodes), err)
		return nil, err
	}

	searchCtx := context.WithoutCancel(ctx)
	helpers.GoSafeNamed("Task.CreateTasks:search", func() { domain.CreateTasksInOpenSearch(searchCtx, search) })
	me := user.UserDgraphInfo
	/* AI call omitted in v1: each task's embedding */
	var top []*dgraphStruct.DgraphTask
	for i, t := range tasks {
		if t.Parent < 0 {
			top = append(top, nodes[i])
		}
	}
	helpers.GoSafeNamed("Task.CreateTasks:tell", func() { tellOfNewTasks(project.Uuid, top, me) })
	eventCtx := webhookBusiness.OfOpenTasks(context.WithoutCancel(ctx))
	helpers.GoSafeNamed("Task.CreateTasks:events", func() {
		for i, t := range tasks {
			if t.Parent >= 0 {
				continue
			}
			webhookBusiness.DispatchEvent(eventCtx, "task.created", map[string]interface{}{
				"task_id":     ids[i].String(),
				"project_id":  projectUUID.String(),
				"name":        t.Input.TaskName,
				"description": t.Input.TaskDescription,
				"status":      t.Input.Status,
				"priority":    t.Input.Priority,
				"created_by":  me.Uuid,
			})
		}
	})
	return ids, nil
}

// newTaskNodes is the graph nodes and search documents of the batch, each
// task a blank node ("_:t0", ...): its statuses resolved once each, its
// dates read, a sub-task hung from its parent in the batch. Nothing is
// written.
func newTaskNodes(ctx context.Context, projectUUID uuid.UUID, user *model.UserInfo, project *dgraphStruct.DgraphProject, tasks []NewTask, now time.Time) ([]*dgraphStruct.DgraphTask, []*openSearchStruct.OpenSearchTask, error) {
	statuses := map[string]taskStatusBusiness.Resolved{}
	zero := time.Time{}
	me := &dgraphStruct.DgraphUser{Uid: user.UserDgraphInfo.Uid}
	team := &dgraphStruct.DgraphTeam{Uid: project.Team.Uid}
	nodes := make([]*dgraphStruct.DgraphTask, len(tasks))
	search := make([]*openSearchStruct.OpenSearchTask, len(tasks))
	for i, t := range tasks {
		in := t.Input
		if t.Parent >= i {
			return nil, nil, fmt.Errorf("business/CreateTasks: task %d sits under %d, which comes after it", i, t.Parent)
		}
		key := strings.TrimSpace(in.Status)
		st, ok := statuses[key]
		if !ok {
			var err error
			if st, err = taskStatusBusiness.Resolve(ctx, projectUUID.String(), key); err != nil {
				return nil, nil, err
			}
			statuses[key] = st
		}
		priority := in.Priority
		if priority == "" {
			priority = dgraphStruct.TASK_PRIORITY_MEDIUM
		}
		label := helpers.NormaliseTags(in.Label)
		desc := in.TaskDescription
		// A millisecond apart, as tasks made one after another are.
		at := now.Add(time.Duration(i) * time.Millisecond)
		n := &dgraphStruct.DgraphTask{
			Uid:         fmt.Sprintf("_:t%d", i),
			Uuid:        uuid.NewString(),
			Project:     &dgraphStruct.DgraphProject{Uid: project.Uid},
			Team:        team,
			Mentions:    &dgraphStruct.DgraphMentions{CreatedAt: &at, DType: []string{"Mention"}},
			StartDate:   &zero,
			DueDate:     &zero,
			Name:        in.TaskName,
			Description: &desc,
			Status:      st.Category,
			StatusSince: &at,
			Priority:    priority,
			Label:       &label,
			CreatedBy:   me,
			Activity: []*dgraphStruct.DgraphTaskActivity{{
				DType: []string{"activity"}, Uuid: uuid.NewString(), Type: dgraphStruct.ACTIVITY_TYPE_CREATE_TASK, CreatedBy: me, LogTime: &at,
			}},
			CreatedAt: &at,
			UpdatedAt: &at,
			DeletedAt: &zero,
		}
		n.Mentions.TaskUuid, n.Mentions.Task = n.Uuid, &dgraphStruct.DgraphTask{Uid: n.Uid}
		if st.CustomID != "" {
			n.CustomStatus, n.CustomStatusName = &st.CustomID, &st.CustomName
		}
		for _, d := range []struct {
			raw string
			to  **time.Time
		}{{in.DueDate, &n.DueDate}, {in.StartDate, &n.StartDate}} {
			if d.raw == "" {
				continue
			}
			v, err := time.Parse(time.RFC3339, d.raw)
			if err != nil {
				return nil, nil, fmt.Errorf("business/CreateTasks: task %d's date %q: %w", i, d.raw, err)
			}
			*d.to = &v
		}
		if t.Parent >= 0 {
			// A sub-task: under its parent, which says so in its history.
			parent := nodes[t.Parent]
			n.ParentTask = &dgraphStruct.DgraphTask{Uid: parent.Uid}
			parent.SubTasks = append(parent.SubTasks, &dgraphStruct.DgraphTask{Uid: n.Uid})
			parent.Activity = append(parent.Activity, &dgraphStruct.DgraphTaskActivity{
				DType: []string{"activity"}, Uuid: uuid.NewString(), Type: dgraphStruct.ACTIVITY_TYPE_ADD_SUB_TASK, CreatedBy: me, NextState: in.TaskName, LogTime: &at,
			})
		}
		nodes[i] = n
		none := ""
		search[i] = &openSearchStruct.OpenSearchTask{
			Uuid: n.Uuid, TaskName: n.Name, TaskDescription: n.Description, TaskAssigneeUuid: &none, TaskCreatedAt: at.Unix(),
			TaskProjectUuid: project.Uuid, TaskProjectName: project.Name, TaskStatus: n.Status, TaskPriority: n.Priority,
			TaskLabel: n.Label, TaskCreatedByUserUuid: user.UserDgraphInfo.Uuid,
		}
	}
	return nodes, search, nil
}

// createOneByOne makes the batch a task at a time, as a person would: for a
// copy that may have to be kept to another task's people.
func createOneByOne(ctx context.Context, projectUUID uuid.UUID, user *model.UserInfo, project *dgraphStruct.DgraphProject, tasks []NewTask) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, len(tasks))
	for _, t := range tasks {
		if t.Parent >= 0 {
			return ids, fmt.Errorf("business/CreateTasks: a copy's sub-tasks are made with CreateSubTask")
		}
		id, err := CreateTask(ctx, projectUUID, user, project, nil, t.Input, nil)
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// tellOfNewTasks tells the project's people who hear of new tasks, once for
// the batch: one task as CreateTask says it, more as "Sam added 12 tasks"
// with the first few names.
func tellOfNewTasks(projectID string, tasks []*dgraphStruct.DgraphTask, maker dgraphStruct.DgraphUser) {
	if len(tasks) == 0 {
		return
	}
	if len(tasks) == 1 {
		desc := ""
		if tasks[0].Description != nil {
			desc = *tasks[0].Description
		}
		sendNewTaskNotification(desc, projectID, tasks[0].Name, nil, tasks[0].Uuid, maker, nil)
		return
	}
	ctx := context.Background()
	// New tasks are nobody's restricted ones, so who may read the first may read them all.
	hearers := newTaskHearers(ctx, projectID, tasks[0].Uuid, maker.Uuid, nil)
	if len(hearers) == 0 {
		return
	}
	names := make([]string, 0, 3)
	for _, t := range tasks {
		if len(names) == 3 {
			break
		}
		names = append(names, t.Name)
	}
	body := strings.Join(names, ", ")
	if len(tasks) > len(names) {
		body += fmt.Sprintf(" and %d more", len(tasks)-len(names))
	}
	notificationBusiness.Deliver(ctx, hearers, map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:     firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:  tasks[0].Uuid,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:    fmt.Sprintf("%s added %d tasks", maker.DisplayName(), len(tasks)),
		firebaseInit.FIREBASE_PUSH_DATA_BODY:     body,
		firebaseInit.FIREBASE_PUSH_DATA_USERNAME: maker.DisplayName(),
		firebaseInit.FIREBASE_PUSH_DATA_ICON:     userBusiness.GetSignedProfileURL(ctx, maker.ProfileKey),
	}, "tasks:"+tasks[0].Uuid)
}
