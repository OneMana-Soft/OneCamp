package business

// The workload: who has how much to do each week, across every project the
// person is in. A task counts for whoever it's assigned to in each week it
// runs, from its start to its due date; their capacity is how many tasks a
// week they take on. Asana keeps this view for its Advanced plan, monday for
// Pro, and ClickUp for its paid plans.

import (
	"context"
	"errors"
	"sort"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Project"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// DefaultWeeklyCapacity is how many tasks a week someone takes on when nobody
// has said: one a working day.
const DefaultWeeklyCapacity = 5

// MaxWeeklyCapacity is the most anyone can set.
const MaxWeeklyCapacity = 100

// maxWorkloadTasks bounds the tasks one workload sends; the latest due stay.
const maxWorkloadTasks = 5000

var (
	ErrCapacityRange    = errors.New("capacity must be from 1 to 100 tasks a week")
	ErrCapacityNotYours = errors.New("only they or a workspace admin can change it")
	ErrPersonNotFound   = errors.New("person not found")
)

// WorkloadPerson is a row of the workload: someone in one of the person's
// projects, or with a task in one, and how many tasks a week they take on.
// project_uuids are the projects they're in (a member or an admin): the view
// keeps to the projects shown with them, and hands a task only to someone in
// its project, which the assignee change itself doesn't check.
type WorkloadPerson struct {
	UUID        string   `json:"user_uuid"`
	Name        string   `json:"user_name"`
	FullName    string   `json:"user_full_name,omitempty"`
	ProfileKey  *string  `json:"user_profile_object_key,omitempty"`
	Title       string   `json:"user_job_title,omitempty"`
	Capacity    int      `json:"capacity"`
	CapacitySet bool     `json:"capacity_set"`
	CanEdit     bool     `json:"can_edit_capacity"`
	Projects    []string `json:"project_uuids"`

	inProject map[string]bool
}

// UndatedCount is how many open tasks of a project someone has with no dates,
// which no week can show; user_uuid is left out for nobody's.
type UndatedCount struct {
	ProjectUUID string `json:"project_uuid"`
	UserUUID    string `json:"user_uuid,omitempty"`
	Count       int    `json:"count"`
}

// WorkloadTask is an open task with dates, as the workload places it. Dates
// not set are left out. can_edit says whether the reader may move it or give
// it to someone else (its project's admins, as for every task edit).
type WorkloadTask struct {
	UUID         string     `json:"task_uuid"`
	Name         string     `json:"task_name"`
	Status       string     `json:"task_status"`
	CustomStatus *string    `json:"task_custom_status,omitempty"`
	CustomName   *string    `json:"task_custom_status_name,omitempty"`
	Start        *time.Time `json:"task_start_date,omitempty"`
	Due          *time.Time `json:"task_due_date,omitempty"`
	AssigneeUUID string     `json:"assignee_uuid,omitempty"`
	ProjectUUID  string     `json:"project_uuid"`
	ProjectName  string     `json:"project_name"`
	ParentName   string     `json:"parent_name,omitempty"`
	CanEdit      bool       `json:"can_edit"`
}

// Workload is what the workload view draws. A task whose assignee's account
// was deleted counts as unassigned: someone has to take it on.
type Workload struct {
	People          []*WorkloadPerson `json:"people"`
	Tasks           []WorkloadTask    `json:"tasks"`
	Undated         []UndatedCount    `json:"undated"`
	DefaultCapacity int               `json:"default_capacity"`
	Truncated       bool              `json:"truncated"`
}

func datedOrNil(t *time.Time) *time.Time {
	if t == nil || t.Year() <= 1970 {
		return nil
	}
	return t
}

// GetWorkload reads the workload of every project the reader is in, for the
// tasks that start before until. A bot isn't a row, and a task an agent has
// is the agent's to run, so it isn't counted.
func GetWorkload(ctx context.Context, readerUID, readerUUID string, readerIsAdmin bool, until time.Time) (*Workload, error) {
	projects, err := domain.GetDgraphWorkload(ctx, readerUID, until)
	if err != nil {
		return nil, err
	}
	people := map[string]*WorkloadPerson{}
	w := &Workload{People: []*WorkloadPerson{}, Tasks: []WorkloadTask{}, Undated: []UndatedCount{}, DefaultCapacity: DefaultWeeklyCapacity}
	add := func(u *dgraphStruct.DgraphUser, project string, member bool) *WorkloadPerson {
		if u == nil || u.Uuid == "" || u.IsBot || datedOrNil(u.DeletedAt) != nil {
			return nil
		}
		p, ok := people[u.Uuid]
		if !ok {
			p = &WorkloadPerson{UUID: u.Uuid, Name: u.UserName, FullName: u.UserFullName, ProfileKey: u.ProfileKey, Title: u.Title,
				Capacity: DefaultWeeklyCapacity, CanEdit: readerIsAdmin || u.Uuid == readerUUID, Projects: []string{}, inProject: map[string]bool{}}
			if u.WeeklyCapacity != nil && *u.WeeklyCapacity > 0 {
				p.Capacity, p.CapacitySet = *u.WeeklyCapacity, true
			}
			people[u.Uuid] = p
			w.People = append(w.People, p)
		}
		if member && !p.inProject[project] {
			p.inProject[project] = true
			p.Projects = append(p.Projects, project)
		}
		return p
	}
	for _, pr := range projects {
		for _, u := range pr.Members {
			add(u, pr.UUID, true)
		}
		for _, u := range pr.Admins {
			add(u, pr.UUID, true)
		}
		if pr.Truncated {
			w.Truncated = true
		}
		for _, t := range pr.Dated {
			if t.Assignee != nil && t.Assignee.IsBot {
				continue
			}
			task := WorkloadTask{UUID: t.Uuid, Name: t.Name, Status: t.Status, CustomStatus: t.CustomStatus, CustomName: t.CustomStatusName,
				Start: datedOrNil(t.StartDate), Due: datedOrNil(t.DueDate), ProjectUUID: pr.UUID, ProjectName: pr.Name, CanEdit: pr.IsAdmin > 0}
			if p := add(t.Assignee, pr.UUID, false); p != nil {
				task.AssigneeUUID = p.UUID
			}
			if t.ParentTask != nil {
				task.ParentName = t.ParentTask.Name
			}
			w.Tasks = append(w.Tasks, task)
		}
	}
	// Who has the tasks without dates, by project, once everyone is known.
	for _, pr := range projects {
		counts := map[string]int{}
		var order []string
		for _, t := range pr.Undated {
			// As for dated tasks: an agent's isn't counted, a deleted account's
			// is nobody's, and someone who left the project keeps a row.
			if t.Assignee != nil && t.Assignee.IsBot {
				continue
			}
			who := ""
			if p := add(t.Assignee, pr.UUID, false); p != nil {
				who = p.UUID
			}
			if counts[who] == 0 {
				order = append(order, who)
			}
			counts[who]++
		}
		for _, who := range order {
			w.Undated = append(w.Undated, UndatedCount{ProjectUUID: pr.UUID, UserUUID: who, Count: counts[who]})
		}
	}
	sort.SliceStable(w.Tasks, func(i, j int) bool { return lastOf(w.Tasks[i]).After(lastOf(w.Tasks[j])) })
	if len(w.Tasks) > maxWorkloadTasks {
		w.Tasks, w.Truncated = w.Tasks[:maxWorkloadTasks], true
	}
	sort.SliceStable(w.People, func(i, j int) bool { return w.People[i].Name < w.People[j].Name })
	return w, nil
}

// lastOf is the day a task is done by: its due date, else its start.
func lastOf(t WorkloadTask) time.Time {
	if t.Due != nil {
		return *t.Due
	}
	if t.Start != nil {
		return *t.Start
	}
	return time.Time{}
}

// SetWeeklyCapacity sets how many tasks a week someone takes on; 0 puts back
// the default. Anyone can set their own, and a workspace admin anyone's.
func SetWeeklyCapacity(ctx context.Context, readerUUID string, readerIsAdmin bool, userUUID string, tasks int) error {
	if userUUID != readerUUID && !readerIsAdmin {
		return ErrCapacityNotYours
	}
	if tasks < 0 || tasks > MaxWeeklyCapacity {
		return ErrCapacityRange
	}
	found, err := domain.SetDgraphWeeklyCapacity(ctx, userUUID, tasks)
	if err != nil {
		return err
	}
	if !found {
		return ErrPersonNotFound
	}
	return nil
}
