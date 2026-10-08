package business

import (
	"context"
	"strings"
	"time"

	taskAdapter "github.com/akashc777/OneCamp/adapter/Task"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Start is when a project made from a template starts. Day is that calendar
// day (midnight UTC, used only for its date) and Loc the creator's zone. The
// days are counted on the calendar, not on a clock in Loc: in a zone whose
// clocks skip midnight (Santiago, Havana, the Azores) local midnight on the
// change day doesn't exist, and counting from it lands every date a day
// early. With SkipWeekends a date that falls on a Saturday or a Sunday moves
// to the Monday after.
type Start struct {
	Day          time.Time
	Loc          *time.Location
	SkipWeekends bool
}

// StartOn is the start for a date written YYYY-MM-DD in the named zone; an
// empty or unreadable date is today there. Pure but for the clock.
func StartOn(date, zone string, skipWeekends bool) Start {
	loc := helpers.Location(zone)
	d, err := time.Parse(time.DateOnly, strings.TrimSpace(date))
	if err != nil {
		y, m, dd := time.Now().In(loc).Date()
		d = time.Date(y, m, dd, 0, 0, 0, 0, time.UTC)
	}
	return Start{Day: d, Loc: loc, SkipWeekends: skipWeekends}
}

// At is the moment a template day falls on, at hour o'clock in the creator's
// zone, in RFC 3339 as a task's dates are written. Pure.
func (s Start) At(day, hour int) string {
	d := s.Day.AddDate(0, 0, day)
	if s.SkipWeekends {
		switch d.Weekday() {
		case time.Saturday:
			d = d.AddDate(0, 0, 2)
		case time.Sunday:
			d = d.AddDate(0, 0, 1)
		}
	}
	loc := s.Loc
	if loc == nil {
		loc = time.UTC
	}
	return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, loc).Format(time.RFC3339)
}

// A task made from a template starts at the beginning of its day and is due
// at the end of the working day.
const (
	startHour = 9
	dueHour   = 17
)

// Applied is what starting a project from a template made.
type Applied struct {
	Statuses int `json:"statuses"`
	Fields   int `json:"fields"`
	Tasks    int `json:"tasks"`  // tasks and subtasks
	Failed   int `json:"failed"` // tasks and subtasks that couldn't be made
}

// Apply makes a template's statuses and tasks in a project just made from it,
// as the person making it. Each task is made the way a person makes one
// (business/Task), unassigned, so search, the AI's memory and the activity
// all see it. One that can't be made is counted and skipped, so a bad row
// never loses the rest.
func Apply(ctx context.Context, t Template, project *dgraphStruct.DgraphProject, user *userModels.UserInfo, start Start) Applied {
	var out Applied
	projectID, err := uuid.Parse(project.Uuid)
	if err != nil {
		out.Failed = t.Size()
		return out
	}
	userID, _ := uuid.Parse(user.UserDgraphInfo.Uuid)

	// A status that couldn't be made leaves its tasks in the built-in status
	// it counts as.
	fallback := map[string]string{}
	for _, st := range t.Statuses {
		if _, err := taskStatusBusiness.Create(ctx, projectID, userID, st); err != nil {
			helpers.LogErrorWithContext(ctx, "business/ProjectTemplate/Apply status %q err: %+v", st.Name, err)
			fallback[strings.ToLower(st.Name)] = st.Category
			continue
		}
		out.Statuses++
	}
	// A field that couldn't be made is left out; the tasks don't need it.
	for _, f := range t.Fields {
		if _, err := taskFieldBusiness.Create(ctx, projectID, f, userID, ""); err != nil {
			helpers.LogErrorWithContext(ctx, "business/ProjectTemplate/Apply field %q err: %+v", f.Name, err)
			continue
		}
		out.Fields++
	}

	var made []string // in the order the template reads: each task, then its subtasks
	for _, task := range t.Tasks {
		in := taskAdapter.CreateOrUpdateTaskInput{
			TaskName:        task.Name,
			TaskDescription: task.Description,
			ProjectUuid:     project.Uuid,
			Status:          task.Status,
			Priority:        task.Priority,
			Label:           task.Tags,
		}
		if c := fallback[strings.ToLower(task.Status)]; c != "" {
			in.Status = c
		}
		if task.StartDay != nil {
			in.StartDate = start.At(*task.StartDay, startHour)
		}
		if task.DueDay != nil {
			in.DueDate = start.At(*task.DueDay, dueHour)
		}
		taskID, err := taskBusiness.CreateTask(ctx, projectID, user, project, nil, in, nil)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/ProjectTemplate/Apply task %q err: %+v", task.Name, err)
			out.Failed += 1 + len(task.Subtasks)
			continue
		}
		made = append(made, taskID.String())
		if len(task.Subtasks) > 0 {
			subs, failed := applySubtasks(ctx, task.Subtasks, projectID, taskID, user, start)
			made = append(made, subs...)
			out.Failed += failed
		}
	}
	out.Tasks = len(made)
	// A project's list and board show the newest first, and tasks made one
	// after another would read bottom to top, each subtask above its task. So
	// the tasks are dated to read as the template does.
	if err := taskDomain.SetDgraphTaskCreatedAt(ctx, readingOrder(made, time.Now())); err != nil {
		helpers.LogErrorWithContext(ctx, "business/ProjectTemplate/Apply order err: %+v", err)
	}
	return out
}

// readingOrder dates tasks a millisecond apart, the first newest, so a
// newest-first list shows them in the order given. Pure.
func readingOrder(ids []string, now time.Time) []*dgraphStruct.DgraphTask {
	out := make([]*dgraphStruct.DgraphTask, len(ids))
	for i, id := range ids {
		at := now.Add(-time.Duration(i) * time.Millisecond)
		out[i] = &dgraphStruct.DgraphTask{Uuid: id, CreatedAt: &at}
	}
	return out
}

// ApplyTo makes the template in the project with that id, read back as the
// person who has just made it. It carries on if the request that asked for it
// goes away, so a project is never left half made.
func ApplyTo(ctx context.Context, t Template, projectUUID string, user *userModels.UserInfo, start Start) Applied {
	ctx = context.WithoutCancel(ctx)
	project, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, projectUUID, user.UserDgraphInfo.Uid)
	if err != nil || project == nil || project.Uid == "" || project.Team == nil {
		helpers.LogErrorWithContext(ctx, "business/ProjectTemplate/ApplyTo project %s err: %+v", projectUUID, err)
		return Applied{Failed: t.Size()}
	}
	return Apply(ctx, t, project, user, start)
}

// applySubtasks makes a task's subtasks, in order, under it, and says which
// it made.
func applySubtasks(ctx context.Context, subs []Subtask, projectID, parentID uuid.UUID, user *userModels.UserInfo, start Start) (made []string, failed int) {
	// A subtask is made under the task as the project reads it back.
	parent, err := projectBusiness.GetBasicDgraphProjectInfoWithTaskUUID(ctx, projectID.String(), user.UserDgraphInfo.Uid, parentID.String())
	if err != nil || parent == nil || len(parent.Tasks) == 0 {
		helpers.LogErrorWithContext(ctx, "business/ProjectTemplate/Apply parent %s not found err: %+v", parentID, err)
		return nil, len(subs)
	}
	for _, sub := range subs {
		in := taskAdapter.CreateOrUpdateTaskInput{Uuid: parentID.String(), TaskName: sub.Name, ProjectUuid: projectID.String()}
		if sub.DueDay != nil {
			in.DueDate = start.At(*sub.DueDay, dueHour)
		}
		task, err := taskBusiness.CreateSubTask(ctx, projectID, user, parent, nil, in)
		if err != nil || task == nil {
			helpers.LogErrorWithContext(ctx, "business/ProjectTemplate/Apply subtask %q err: %+v", sub.Name, err)
			failed++
			continue
		}
		made = append(made, task.Uuid)
	}
	return made, failed
}
