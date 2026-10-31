package Calendar

// What repeats on a person's calendar beside their events (calendar head 9,
// gcal-1b): the check-ins of the channels they are in, at each time they ask,
// and their open tasks that repeat on a schedule, on each date the next ones
// will be due when each is done on time. Repeating meetings are the events'
// own series (series.go); these two are worked out from their own rules
// (business/CheckIn's Next, business/Task's NextOccurrence), never stored.
//
// Only what is the person's own: a channel they are in (their session's list,
// so a workspace admin sees the channels they are in, as they are asked
// there), and a task assigned to them that their task list shows them
// (business/User GetDgraphUserTaskList, its archived-project and restricted
// rules included). An API token acts as its member, as /calendar/events does.

import (
	"context"
	"sort"
	"time"

	checkInBusiness "github.com/akashc777/OneCamp/business/CheckIn"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	recurrenceModel "github.com/akashc777/OneCamp/models/postgres/TaskRecurrence"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// MaxRepeatsSpan is the longest stretch one read covers: a month's six weeks
// with room to spare.
const MaxRepeatsSpan = 62 * 24 * time.Hour

// maxRepeatTimes is the most times one check-in or one task is listed in a
// read: a daily one over the longest stretch.
const maxRepeatTimes = 100

// RepeatsView is what repeats on a person's calendar in a stretch of time.
type RepeatsView struct {
	CheckIns []CheckInAsks `json:"checkins"`
	Tasks    []TaskAhead   `json:"tasks"`
}

// CheckInAsks is a channel's check-in and each time it asks.
type CheckInAsks struct {
	ID          string      `json:"id"`
	ChannelUUID string      `json:"channel_uuid"`
	ChannelName string      `json:"channel_name"`
	Question    string      `json:"question"`
	At          []time.Time `json:"at"`
}

// TaskAhead is a task that repeats on a schedule and the dates its next ones
// will be due. The task as it is now is on the calendar already, by its own
// due date.
type TaskAhead struct {
	TaskUUID    string      `json:"task_uuid"`
	Name        string      `json:"task_name"`
	ProjectUUID string      `json:"project_uuid,omitempty"`
	ProjectName string      `json:"project_name,omitempty"`
	Rule        string      `json:"rule"`
	Due         []time.Time `json:"due"`
}

// The reads, as variables a test can stand in for.
var (
	checkInAsks      = checkInBusiness.AsksBetween
	scheduledRepeats = recurrenceModel.ScheduledFor
	openTasksOf      = func(ctx context.Context, u *model.UserInfo, filter string) ([]*dgraphStruct.DgraphTask, error) {
		user, err := userBusiness.GetDgraphUserTaskList(ctx, u.UserDgraphInfo.Uuid, u.UserDgraphInfo.Uid, filter, "", 0, 0, true)
		if err != nil || user == nil {
			return nil, err
		}
		return user.Tasks, nil
	}
)

// Repeats is what repeats on u's calendar from `from` until before `to`, as
// of now: three reads (the check-ins of u's channels, u's open tasks, and
// which of them repeat on a schedule).
func Repeats(ctx context.Context, u *model.UserInfo, from, to, now time.Time) (*RepeatsView, error) {
	view := &RepeatsView{CheckIns: []CheckInAsks{}, Tasks: []TaskAhead{}}

	names := map[uuid.UUID]string{}
	var channels []uuid.UUID
	for _, ch := range u.UserDgraphInfo.Channels {
		if ch == nil {
			continue
		}
		id, err := uuid.Parse(ch.Uuid)
		if err != nil {
			continue
		}
		if _, seen := names[id]; !seen {
			channels = append(channels, id)
		}
		names[id] = ch.Name
	}
	asks, err := checkInAsks(channels, from, to, maxRepeatTimes)
	if err != nil {
		return nil, err
	}
	for _, a := range asks {
		view.CheckIns = append(view.CheckIns, CheckInAsks{
			ID: a.ID.String(), ChannelUUID: a.Channel.String(), ChannelName: names[a.Channel], Question: a.Question, At: a.At,
		})
	}

	// The asker's open tasks first (their own task list), then which of
	// them repeat on a schedule: one read each, however many repeats the
	// workspace has.
	tasks, err := openTasksOf(ctx, u, dgraphStruct.TASK_OPEN_FILTER)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t != nil && t.Uuid != "" && t.DueDate != nil {
			ids = append(ids, t.Uuid)
		}
	}
	if len(ids) == 0 {
		return view, nil
	}
	repeats, err := scheduledRepeats(ids)
	if err != nil {
		return nil, err
	}
	byTask := make(map[string]recurrenceModel.TaskRecurrence, len(repeats))
	for _, r := range repeats {
		byTask[r.TaskUUID.String()] = r
	}
	for _, t := range tasks {
		if t == nil {
			continue
		}
		r, ok := byTask[t.Uuid]
		if !ok {
			continue
		}
		due := taskBusiness.DueAhead(r, t.DueDate, now, from, to, maxRepeatTimes)
		if len(due) == 0 {
			continue
		}
		ahead := TaskAhead{TaskUUID: t.Uuid, Name: t.Name, Rule: r.Rule, Due: due}
		if t.Project != nil {
			ahead.ProjectUUID, ahead.ProjectName = t.Project.Uuid, t.Project.Name
		}
		view.Tasks = append(view.Tasks, ahead)
	}
	sort.SliceStable(view.Tasks, func(i, j int) bool { return view.Tasks[i].Due[0].Before(view.Tasks[j].Due[0]) })
	return view, nil
}
