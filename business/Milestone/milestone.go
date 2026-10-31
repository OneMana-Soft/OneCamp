// Package business holds milestones (linear-16): the stages of a project's
// work. A milestone has a name, a few words on what it means, an optional
// target day and its tasks. Its progress counts them as a cycle's does
// (Cycle's Count), it spans its tasks' days, and the first one in a
// project's order that isn't finished is the one to focus on. It belongs to
// the project that made it, whose admins change it, and can be shared with
// other projects, whose tasks then join it: one launch across the projects
// that ship it, which Linear can't do.
//
// A reader sees a milestone's progress and span over the tasks they may
// open, in the projects it shows in that they may open, and those projects
// by name; any other project it shows in is only counted.
//
// Milestones stay in Postgres beside the graph's projects and tasks, joined
// by uuid as cycles are: nothing reads them from the graph, and a task's
// milestone is read with its project's list.
package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	cycleBusiness "github.com/akashc777/OneCamp/business/Cycle"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	milestoneModel "github.com/akashc777/OneCamp/models/postgres/Milestone"
	"github.com/google/uuid"
)

// MilestoneError is a request the person can fix; its text is written for them.
type MilestoneError struct{ msg string }

func (e *MilestoneError) Error() string { return e.msg }

const (
	// MostPerProject is the most milestones a project shows, its own and
	// those shared with it.
	MostPerProject = 50
	// MostProjects is the most projects one milestone shows in, its home
	// among them.
	MostProjects = 10
	// nameMost and descriptionMost are migration 935's column sizes.
	nameMost        = 80
	descriptionMost = 2000
	dayLayout       = "2006-01-02"
)

// Input is a milestone's words and target day as a person sets them.
type Input struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// TargetDate is a day, "2006-01-02"; null or "" for none.
	TargetDate *string `json:"target_date"`
}

// Clean is in as a milestone keeps it, or why it can't: the name on one line
// and both in bounds, the day a real one. Pure.
func (in Input) Clean() (Input, error) {
	out := Input{Name: strings.Join(strings.Fields(in.Name), " "), Description: strings.TrimSpace(in.Description)}
	switch n := utf8.RuneCountInString(out.Name); {
	case n == 0:
		return out, &MilestoneError{"Name the milestone."}
	case n > nameMost:
		return out, &MilestoneError{fmt.Sprintf("Keep the name to %d characters.", nameMost)}
	}
	if utf8.RuneCountInString(out.Description) > descriptionMost {
		return out, &MilestoneError{"Keep the description to 2,000 characters."}
	}
	if strings.ContainsRune(out.Name+out.Description, 0) {
		return out, &MilestoneError{"That text has a character a milestone can't keep."}
	}
	if in.TargetDate != nil && strings.TrimSpace(*in.TargetDate) != "" {
		d, err := time.Parse(dayLayout, strings.TrimSpace(*in.TargetDate))
		if err != nil || d.Year() < 1970 {
			return out, &MilestoneError{"That target date couldn't be read."}
		}
		day := d.Format(dayLayout)
		out.TargetDate = &day
	}
	return out, nil
}

// Progress is a milestone's tasks counted by where they stand, as a cycle's
// are.
type Progress = cycleBusiness.Progress

// Finished is whether a milestone with progress p is done: it has tasks, and
// every one is closed. Pure.
func Finished(p Progress) bool { return p.Total > 0 && p.Done == p.Total }

// ProjectRef is a project a milestone shows in, one its reader may open.
type ProjectRef struct {
	UUID string `json:"project_uuid"`
	Name string `json:"project_name"`
}

// View is a milestone as one reader sees it in one project's list.
type View struct {
	milestoneModel.Milestone
	Progress Progress `json:"progress"`
	// Starts and Ends run from its tasks' first day to their last (a task's
	// start, or its due day when it has none), nil when none is dated.
	Starts *time.Time `json:"starts,omitempty"`
	Ends   *time.Time `json:"ends,omitempty"`
	// Home is whether the list's project made it. Projects are the projects
	// it shows in that the reader may open, its home first when it is one;
	// HiddenProjects counts the rest.
	Home           bool         `json:"home"`
	Projects       []ProjectRef `json:"projects"`
	HiddenProjects int          `json:"hidden_projects"`
	// Next marks the first in the list's order that isn't finished.
	Next bool `json:"next"`
	// CanEdit is whether the reader runs its home project, whose admins
	// rename, date, share and delete it.
	CanEdit bool `json:"can_edit"`
}

// fact is one of a milestone's tasks as its reader may count it.
type fact struct {
	status, project string // project: a lower-case uuid
	start, due      *time.Time
}

// stand is how the reader stands in a project a milestone shows in.
type stand struct {
	name         string
	open, manage bool
}

// views is the milestones a project shows (ms, in its order) as one reader
// sees them, from the tasks they may count (facts) and how they stand in
// each project (stands); members are each milestone's tasks. Pure.
func views(project uuid.UUID, ms []milestoneModel.Milestone, members map[uuid.UUID][]string, facts map[string]fact, stands map[string]stand) []View {
	out := make([]View, 0, len(ms))
	focused := false
	for _, m := range ms {
		v := View{Milestone: m, Home: m.ProjectUUID == project, Projects: []ProjectRef{}, CanEdit: stands[key(m.ProjectUUID)].manage}
		shownIn := map[string]bool{}
		for _, p := range m.Projects {
			k := key(p)
			if s := stands[k]; s.open {
				shownIn[k] = true
				v.Projects = append(v.Projects, ProjectRef{UUID: k, Name: s.name})
			} else {
				v.HiddenProjects++
			}
		}
		var statuses []string
		for _, t := range members[m.Id] {
			f, ok := facts[strings.ToLower(t)]
			// A task the reader can't open, or one whose project the
			// milestone was taken off, doesn't count.
			if !ok || !shownIn[f.project] {
				continue
			}
			statuses = append(statuses, f.status)
			v.Starts, v.Ends = widen(v.Starts, v.Ends, f)
		}
		v.Progress = cycleBusiness.Count(statuses)
		if !focused && !Finished(v.Progress) {
			v.Next, focused = true, true
		}
		out = append(out, v)
	}
	return out
}

// widen stretches a span from starts to ends over task f's days. Pure.
func widen(starts, ends *time.Time, f fact) (*time.Time, *time.Time) {
	first, last := f.start, f.due
	if first == nil {
		first = f.due
	}
	if last == nil {
		last = f.start
	}
	if first == nil {
		return starts, ends
	}
	if starts == nil || first.Before(*starts) {
		starts = first
	}
	if ends == nil || last.After(*ends) {
		ends = last
	}
	return starts, ends
}

func key(id uuid.UUID) string { return strings.ToLower(id.String()) }

// Reader is who a list is read for: their user uuid and their graph uid.
type Reader struct{ UUID, UID string }

// List is the milestones project shows, in its order, as reader sees them.
// The caller has checked they may see the project.
func List(ctx context.Context, project uuid.UUID, reader Reader) ([]View, error) {
	shown, err := milestoneModel.In(ctx, []uuid.UUID{project})
	if err != nil {
		return nil, err
	}
	all, err := see(ctx, shown, reader, nil)
	if err != nil {
		return nil, err
	}
	// None is an empty list, never null: a client reads null as not read yet
	// (V-51: the section drew its loading rows for ever).
	if views := all[project]; views != nil {
		return views, nil
	}
	return []View{}, nil
}

// Mine is the milestones of every project reader is in, by project: what the
// projects overview and its timeline draw. Projects they may not open, or
// that show none, are left out.
func Mine(ctx context.Context, reader Reader) (map[string][]View, error) {
	projects, err := projectDomain.GetReaderProjectsReadAccess(ctx, reader.UID)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(projects))
	for _, p := range projects {
		if id, err := uuid.Parse(p.Uuid); err == nil {
			ids = append(ids, id)
		}
	}
	shown, err := milestoneModel.In(ctx, ids)
	if err != nil {
		return nil, err
	}
	all, err := see(ctx, shown, reader, projects)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]View, len(all))
	for p, vs := range all {
		out[key(p)] = vs
	}
	return out, nil
}

// see is each project's milestones (shown) as reader sees them, with one
// read of their tasks and one of the projects they show in; known are
// projects read for reader already.
func see(ctx context.Context, shown map[uuid.UUID][]milestoneModel.Milestone, reader Reader, known []*dgraphStruct.DgraphProject) (map[uuid.UUID][]View, error) {
	var ids []uuid.UUID
	projectIDs := []string{}
	seenM, seenP := map[uuid.UUID]bool{}, map[string]bool{}
	for _, p := range known {
		seenP[strings.ToLower(p.Uuid)] = true
	}
	for _, ms := range shown {
		for _, m := range ms {
			if !seenM[m.Id] {
				seenM[m.Id] = true
				ids = append(ids, m.Id)
			}
			for _, p := range m.Projects {
				if k := key(p); !seenP[k] {
					seenP[k] = true
					projectIDs = append(projectIDs, k)
				}
			}
		}
	}
	out := make(map[uuid.UUID][]View, len(shown))
	if len(ids) == 0 {
		return out, nil
	}
	projects := known
	if len(projectIDs) > 0 {
		more, err := projectDomain.GetProjectsReadAccessFor(ctx, projectIDs, reader.UID)
		if err != nil {
			return nil, err
		}
		projects = append(append([]*dgraphStruct.DgraphProject{}, known...), more...)
	}
	standings, err := workAccess.OfMany(ctx, reader.UUID, projects)
	if err != nil {
		return nil, err
	}
	stands := make(map[string]stand, len(projects))
	for _, p := range projects {
		k := strings.ToLower(p.Uuid)
		s := standings[k]
		live := !s.Archived && !helpers.IsSoftDeleted(p.DeletedAt)
		stands[k] = stand{name: p.Name, open: live && s.Can(workAccess.NeedView), manage: live && s.Can(workAccess.NeedManage)}
	}
	members, err := milestoneModel.Members(ctx, ids)
	if err != nil {
		return nil, err
	}
	var taskIDs []string
	for _, ts := range members {
		taskIDs = append(taskIDs, ts...)
	}
	facts := map[string]fact{}
	for start := 0; start < len(taskIDs); start += readAtOnce {
		tasks, err := taskDomain.GetTasksReadAccessFor(ctx, taskIDs[start:min(start+readAtOnce, len(taskIDs))], reader.UID)
		if err != nil {
			return nil, err
		}
		for _, t := range tasks {
			if t == nil || t.Project == nil || helpers.IsSoftDeleted(t.DeletedAt) {
				continue
			}
			facts[strings.ToLower(t.Uuid)] = fact{status: t.Status, project: strings.ToLower(t.Project.Uuid), start: t.StartDate, due: t.DueDate}
		}
	}
	for project, ms := range shown {
		if stands[key(project)].open {
			out[project] = views(project, ms, members, facts, stands)
		}
	}
	return out, nil
}

// readAtOnce is the most tasks one graph read asks for.
const readAtOnce = 1000

// Put puts tasks (by id, each with its project's id; "" for none) in the
// milestone with id milestoneID, or takes them out of theirs (""), in one
// write however many. A task in no project, or in one the milestone doesn't
// show in, is refused in words (refused); a milestone that isn't there
// refuses them all (a MilestoneError). Moved are the ids it moved; the rest
// were there already.
func Put(ctx context.Context, tasks map[uuid.UUID]string, milestoneID string) (moved []string, refused map[uuid.UUID]error, err error) {
	var target *milestoneModel.Milestone
	if milestoneID != "" {
		if id, perr := uuid.Parse(milestoneID); perr == nil {
			if target, err = milestoneModel.Get(ctx, id); err != nil {
				return nil, nil, err
			}
		}
		if target == nil {
			return nil, nil, &MilestoneError{gone}
		}
	}
	refused = map[uuid.UUID]error{}
	var ids []uuid.UUID
	for t, project := range tasks {
		switch {
		case project == "":
			refused[t] = &MilestoneError{"A task in no project has no milestones."}
		case target != nil && !ShowsIn(target, project):
			refused[t] = &MilestoneError{"That milestone isn't one of this task's project's."}
		default:
			ids = append(ids, t)
		}
	}
	var to *uuid.UUID
	if target != nil {
		to = &target.Id
	}
	moved, err = milestoneModel.SetTasks(ctx, ids, to)
	return moved, refused, worded(err)
}

// ShowsIn is whether milestone m shows in the project with id project. Pure.
func ShowsIn(m *milestoneModel.Milestone, project string) bool {
	for _, p := range m.Projects {
		if strings.EqualFold(p.String(), project) {
			return true
		}
	}
	return false
}

// Of is the milestone the task is in, while its project (the task's, by id)
// still shows it; nil otherwise.
func Of(ctx context.Context, task uuid.UUID, project string) (*milestoneModel.Milestone, error) {
	m, err := milestoneModel.Of(ctx, task)
	if err != nil || m == nil || !ShowsIn(m, project) {
		return nil, err
	}
	return m, nil
}

// worded is err in words for the person when it is theirs to fix: a list
// that is full, or a milestone deleted meanwhile.
func worded(err error) error {
	switch {
	case errors.Is(err, milestoneModel.ErrFull):
		return &MilestoneError{fmt.Sprintf("A project shows at most %d milestones, and a milestone shows in at most %d projects.", MostPerProject, MostProjects)}
	case errors.Is(err, milestoneModel.ErrGone):
		return &MilestoneError{gone}
	}
	return err
}

const gone = "That milestone isn't there any more."

// Create makes a milestone in project, last in its order.
func Create(ctx context.Context, project uuid.UUID, in Input, by uuid.UUID) (*milestoneModel.Milestone, error) {
	in, err := in.Clean()
	if err != nil {
		return nil, err
	}
	m, err := milestoneModel.Create(ctx, project, in.Name, in.Description, in.TargetDate, by, MostPerProject)
	return m, worded(err)
}

// Edit sets milestone m's name, description and target day.
func Edit(ctx context.Context, m *milestoneModel.Milestone, in Input) error {
	in, err := in.Clean()
	if err != nil {
		return err
	}
	return milestoneModel.Edit(ctx, m.Id, in.Name, in.Description, in.TargetDate)
}

// Reorder puts the project's milestones in the order of ids, first to last.
func Reorder(ctx context.Context, project uuid.UUID, ids []string) error {
	if len(ids) > MostPerProject {
		return &MilestoneError{"That's more milestones than a project shows."}
	}
	order := make([]uuid.UUID, 0, len(ids))
	for _, s := range ids {
		id, err := uuid.Parse(s)
		if err != nil {
			return &MilestoneError{"That order names something that isn't a milestone."}
		}
		order = append(order, id)
	}
	return milestoneModel.Reorder(ctx, project, order)
}

// Share shows milestone m in project too. The caller has checked who may.
func Share(ctx context.Context, m *milestoneModel.Milestone, project uuid.UUID) error {
	return worded(milestoneModel.Share(ctx, m.Id, project, MostProjects, MostPerProject))
}

// Unshare takes milestone m off project, with that project's tasks; a
// milestone stays in its home.
func Unshare(ctx context.Context, m *milestoneModel.Milestone, project uuid.UUID) error {
	if project == m.ProjectUUID {
		return &MilestoneError{"A milestone stays in the project that made it: delete it there instead."}
	}
	return milestoneModel.Unshare(ctx, m.Id, project)
}
