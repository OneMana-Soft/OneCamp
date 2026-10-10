package business

// Project guests: the client's view of the work being done for them. A
// project's admin gives someone outside the workspace a link to one project;
// they see its tasks by status, open one for its description, and (with the
// comment capability) read and write its comments.
//
// The same construction as channel guests: a grant, never a user. The read is
// made as the "Guests" principal and filtered to the grant's project, and a
// guest's comment is posted by that principal with the guest's name on it.
// Cancelled tasks stay out of the view; a client has no use for them.

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	updateBusiness "github.com/akashc777/OneCamp/business/ProjectUpdate"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskStatus "github.com/akashc777/OneCamp/business/TaskStatus"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	projectModels "github.com/akashc777/OneCamp/models/dgraph/Project"
	taskModels "github.com/akashc777/OneCamp/models/dgraph/Task"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ClientReview"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// GuestTaskCard is a task on the client's board.
type GuestTaskCard struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Status       string     `json:"status"`
	StatusLabel  string     `json:"status_label"`
	Priority     string     `json:"priority,omitempty"`
	StartDate    *time.Time `json:"start_date,omitempty"`
	DueDate      *time.Time `json:"due_date,omitempty"`
	Assignee     string     `json:"assignee,omitempty"`
	CommentCount uint32     `json:"comment_count"`
	// Review is the client's newest verdict, if they gave one.
	Review *ReviewView `json:"review,omitempty"`
}

// GuestColumn is one status and its tasks, in board order.
type GuestColumn struct {
	Status string          `json:"status"`
	Label  string          `json:"label"`
	Tasks  []GuestTaskCard `json:"tasks"`
}

// GuestProjectView is the whole project as its guest sees it.
type GuestProjectView struct {
	Project     string        `json:"project"`
	CanComment  bool          `json:"can_comment"`
	Columns     []GuestColumn `json:"columns"`
	TotalTasks  int           `json:"total_tasks"`
	DoneTasks   int           `json:"done_tasks"`
	GeneratedAt time.Time     `json:"generated_at"`
	// Updates are the project's updates its team shared with the client,
	// newest first.
	Updates []GuestUpdate `json:"updates"`
}

// GuestUpdate is a project update as its client reads it.
type GuestUpdate struct {
	Health      string    `json:"health"`
	HealthLabel string    `json:"health_label"`
	Body        string    `json:"body"`
	Author      string    `json:"author"`
	CreatedAt   time.Time `json:"created_at"`
}

// guestUpdatesShown is how many shared updates the client's page lists.
const guestUpdatesShown = 10

// GuestTaskView is one task opened by its project's guest.
type GuestTaskView struct {
	GuestTaskCard
	Description string         `json:"description"`
	Comments    []GuestMessage `json:"comments"`
	CanComment  bool           `json:"can_comment"`
}

// guestStatuses is the order a client reads a project in: the built-in
// statuses, named as everywhere else, with cancelled work left out.
var guestStatuses = func() []struct{ status, label string } {
	out := []struct{ status, label string }{}
	for _, b := range taskStatus.BuiltIns {
		if b.Key != dgraphStruct.TASK_STATUS_CANCELED {
			out = append(out, struct{ status, label string }{b.Key, b.Label})
		}
	}
	return out
}()

// StatusLabel is a status as a guest reads it: the team's own name for it when
// they gave one, else the built-in name. Pure.
func StatusLabel(status string, custom *string) string {
	if custom != nil && strings.TrimSpace(*custom) != "" {
		return strings.TrimSpace(*custom)
	}
	for _, s := range guestStatuses {
		if s.status == status {
			return s.label
		}
	}
	return status
}

// dateOf is a task date a guest can read, or nil: the graph keeps "no date"
// as the zero time (or the epoch), which must not show as a deadline. Pure.
func dateOf(t *time.Time) *time.Time {
	if t == nil || t.IsZero() || !t.After(time.Unix(0, 0)) {
		return nil
	}
	return t
}

func cardOf(t *dgraphStruct.DgraphTask) GuestTaskCard {
	c := GuestTaskCard{
		ID:           t.Uuid,
		Name:         t.Name,
		Status:       t.Status,
		StatusLabel:  StatusLabel(t.Status, t.CustomStatusName),
		Priority:     t.Priority,
		StartDate:    dateOf(t.StartDate),
		DueDate:      dateOf(t.DueDate),
		CommentCount: t.CommentCount,
	}
	if t.Assignee != nil {
		c.Assignee = authorName(t.Assignee)
	}
	return c
}

func projectOf(grant *guestModel.GuestGrant) (uuid.UUID, error) {
	if grant == nil || grant.ResourceType != guestModel.ResourceProject {
		return uuid.Nil, ErrForbidden
	}
	return uuid.Parse(grant.ResourceID)
}

// liveProject is the grant's project if it is still there, read as the
// guests' principal.
func liveProject(ctx context.Context, grant *guestModel.GuestGrant) (uuid.UUID, *userBusiness.BotIdentity, error) {
	projectID, err := projectOf(grant)
	if err != nil {
		return uuid.Nil, nil, err
	}
	bot, err := userBusiness.EnsureGuestBot(ctx)
	if err != nil {
		return uuid.Nil, nil, err
	}
	p, err := projectBusiness.GetBasicDgraphProjectInfo(helpers.WithSystemRead(ctx), projectID.String(), bot.DgraphUID)
	if readFailed(err, projectModels.ErrNotFound) {
		return uuid.Nil, nil, err
	}
	if p == nil || p.Uuid == "" || helpers.IsSoftDeleted(p.DeletedAt) {
		return uuid.Nil, nil, ErrNotFound
	}
	return projectID, bot, nil
}

// GetGuestProject is the project's tasks by status.
func GetGuestProject(ctx context.Context, grant *guestModel.GuestGrant) (*GuestProjectView, error) {
	projectID, bot, err := liveProject(ctx, grant)
	if err != nil {
		return nil, err
	}
	p, err := projectBusiness.GetDgraphProjectTaskListForKanban(helpers.WithSystemRead(ctx), projectID.String(), bot.DgraphUID, "", dgraphStruct.BoardClosedLimit)
	if readFailed(err, projectModels.ErrNotFound) {
		return nil, err
	}
	if p == nil {
		return nil, ErrNotFound
	}
	byStatus := map[string][]*dgraphStruct.DgraphTask{
		dgraphStruct.TASK_STATUS_BACKLOG:    p.TasksBacklog,
		dgraphStruct.TASK_STATUS_TODO:       p.TasksTodo,
		dgraphStruct.TASK_STATUS_INPROGRESS: p.TasksInProgresss,
		dgraphStruct.TASK_STATUS_INREVIEW:   p.TasksInReview,
		dgraphStruct.TASK_STATUS_DONE:       p.TasksDone,
	}
	view := &GuestProjectView{
		Project:     p.Name,
		CanComment:  grant.Capability == guestModel.CapabilityComment,
		Columns:     make([]GuestColumn, 0, len(guestStatuses)),
		GeneratedAt: time.Now().UTC(),
	}
	reviews, err := reviewModel.LatestForProject(projectID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Guest/GetGuestProject reviews err: %+v", err)
	}
	for _, s := range guestStatuses {
		col := GuestColumn{Status: s.status, Label: s.label, Tasks: []GuestTaskCard{}}
		for _, t := range byStatus[s.status] {
			if t == nil || t.Uuid == "" {
				continue
			}
			card := cardOf(t)
			if id, err := uuid.Parse(t.Uuid); err == nil {
				if r, ok := reviews[id]; ok {
					card.Review = reviewView(&r)
				}
			}
			col.Tasks = append(col.Tasks, card)
		}
		view.TotalTasks += len(col.Tasks)
		if s.status == dgraphStruct.TASK_STATUS_DONE {
			view.DoneTasks = len(col.Tasks)
			// The board lists the newest finished tasks; the count is all of them.
			if p.TasksDoneCount > view.DoneTasks {
				view.TotalTasks += p.TasksDoneCount - view.DoneTasks
				view.DoneTasks = p.TasksDoneCount
			}
		}
		view.Columns = append(view.Columns, col)
	}
	view.Updates = []GuestUpdate{}
	if shared, err := updateBusiness.List(ctx, projectID, guestUpdatesShown, true); err == nil {
		for _, u := range shared {
			view.Updates = append(view.Updates, GuestUpdate{
				Health: u.Health, HealthLabel: updateBusiness.HealthLabels[u.Health], Body: u.Body, Author: u.GuestAuthorName, CreatedAt: u.CreatedAt,
			})
		}
	} else {
		helpers.LogErrorWithContext(ctx, "business/Guest/GetGuestProject updates err: %+v", err)
	}
	return view, nil
}

// guestTask is a task of the grant's project that a guest may open: there,
// not deleted, not cancelled. Anything else answers as missing.
func guestTask(ctx context.Context, projectID uuid.UUID, taskID string) (*dgraphStruct.DgraphTask, error) {
	id, err := uuid.Parse(strings.TrimSpace(taskID))
	if err != nil {
		return nil, ErrNotFound
	}
	t, err := taskDomain.GetDgraphTaskForGuest(helpers.WithSystemRead(ctx), id.String())
	if readFailed(err, taskModels.ErrNotFound) {
		return nil, err
	}
	if t == nil || t.Uuid == "" || t.Project == nil || t.Project.Uuid != projectID.String() ||
		helpers.IsSoftDeleted(t.DeletedAt) || t.Status == dgraphStruct.TASK_STATUS_CANCELED {
		return nil, ErrNotFound
	}
	return t, nil
}

// GetGuestTask is one task of the guest's project. Comments come only with the
// comment capability: a view link shows the work, not the conversation.
func GetGuestTask(ctx context.Context, grant *guestModel.GuestGrant, taskID string) (*GuestTaskView, error) {
	projectID, _, err := liveProject(ctx, grant)
	if err != nil {
		return nil, err
	}
	t, err := guestTask(ctx, projectID, taskID)
	if err != nil {
		return nil, err
	}
	canComment := grant.Capability == guestModel.CapabilityComment
	v := &GuestTaskView{GuestTaskCard: cardOf(t), Comments: []GuestMessage{}, CanComment: canComment}
	if t.Description != nil {
		v.Description = PlainText(*t.Description)
	}
	v.CommentCount = uint32(len(t.Comments))
	if id, err := uuid.Parse(t.Uuid); err == nil {
		if r, err := reviewModel.Latest(id); err == nil {
			v.Review = reviewView(r)
		}
	}
	if canComment {
		for _, c := range t.Comments {
			v.Comments = append(v.Comments, guestMessage(c.Uuid, c.CommentBy, c.Text, c.CreatedAt))
		}
	}
	return v, nil
}

// CommentAsGuest adds a guest's comment to a task of their project. The team
// is told the way any comment tells them; it never reaches a linked GitHub
// issue, since a client's words are not the team's to publish.
func CommentAsGuest(ctx context.Context, grant *guestModel.GuestGrant, taskID, name, text string) error {
	if grant == nil || grant.Capability != guestModel.CapabilityComment {
		return ErrForbidden
	}
	body, err := GuestMessageHTML(name, text)
	if err != nil {
		return err
	}
	projectID, bot, err := liveProject(ctx, grant)
	if err != nil {
		return err
	}
	if _, err := guestTask(ctx, projectID, taskID); err != nil {
		return err
	}
	return postGuestComment(ctx, bot, taskID, body)
}

// postGuestComment posts already-escaped HTML on a task as the guests'
// principal. The caller has checked the grant and that the task is the
// grant's project's.
func postGuestComment(ctx context.Context, bot *userBusiness.BotIdentity, taskID, body string) error {
	task, err := taskBusiness.GetDgraphBasicTaskInfo(helpers.WithSystemRead(ctx), taskID, bot.DgraphUID)
	if err != nil || task == nil || task.Project == nil {
		return ErrNotFound
	}
	actor := &userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: bot.UserID},
		UserDgraphInfo:   dgraphStruct.DgraphUser{Uid: bot.DgraphUID, Uuid: bot.UUID, UserName: bot.Name},
	}
	if bot.ProfileKey != "" {
		key := bot.ProfileKey
		actor.UserDgraphInfo.ProfileKey = &key
	}
	id, _ := uuid.Parse(task.Uuid)
	_, err = taskBusiness.CreateTaskComment(ctx, id, task, actor, &adapter.CreateOrUpdateTaskCommentInput{
		CommentBody:    body,
		TaskUuid:       task.Uuid,
		SkipGitHubSync: true,
	}, nil)
	return err
}

// ReviewView is a client's verdict on a task, as anyone sees it.
type ReviewView struct {
	Decision  string    `json:"decision"`
	Name      string    `json:"name"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func reviewView(r *reviewModel.Review) *ReviewView {
	if r == nil {
		return nil
	}
	return &ReviewView{Decision: r.Decision, Name: r.Name, Note: r.Note, CreatedAt: r.CreatedAt}
}

const maxReviewNote = 2000

// ReviewText is the comment a verdict posts, and checks it. Pure.
func ReviewText(decision, note string) (string, string, error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > maxReviewNote {
		return "", "", &ErrGuestInput{"Keep the note under 2,000 characters."}
	}
	switch decision {
	case reviewModel.Approved:
		if note == "" {
			return "Approved.", note, nil
		}
		return "Approved. " + note, note, nil
	case reviewModel.Changes:
		if note == "" {
			return "", "", &ErrGuestInput{"Say what should change."}
		}
		return "Changes requested: " + note, note, nil
	}
	return "", "", &ErrGuestInput{"Choose approve or request changes."}
}

// ReviewAsGuest records a client's verdict on a task and posts it as their
// comment, so the people on the task are told.
func ReviewAsGuest(ctx context.Context, grant *guestModel.GuestGrant, taskID, name, decision, note string) (*ReviewView, error) {
	if grant == nil || grant.Capability != guestModel.CapabilityComment {
		return nil, ErrForbidden
	}
	text, note, err := ReviewText(decision, note)
	if err != nil {
		return nil, err
	}
	body, err := GuestMessageHTML(name, text)
	if err != nil {
		return nil, err
	}
	projectID, bot, err := liveProject(ctx, grant)
	if err != nil {
		return nil, err
	}
	task, err := guestTask(ctx, projectID, taskID)
	if err != nil {
		return nil, err
	}
	taskUUID, _ := uuid.Parse(task.Uuid)
	r, err := reviewModel.Add(taskUUID, projectID, grant.Id, decision, SanitizeGuestName(name), note)
	if err != nil {
		return nil, err
	}
	if err := postGuestComment(ctx, bot, task.Uuid, body); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Guest/ReviewAsGuest comment err: %+v", err)
	}
	return reviewView(r), nil
}

// LatestReview is a task's newest client verdict, for the team's task panel.
func LatestReview(taskID uuid.UUID) (*ReviewView, error) {
	r, err := reviewModel.Latest(taskID)
	return reviewView(r), err
}
