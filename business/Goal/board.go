package business

import (
	"context"
	"sort"
	"strings"
	"time"

	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	goalModel "github.com/akashc777/OneCamp/models/postgres/Goal"
	updateModel "github.com/akashc777/OneCamp/models/postgres/ProjectUpdate"
	"github.com/google/uuid"
)

// board is every live goal with what its progress is made of, read once per
// request: the goals, the projects serving them (counted as the reader sees
// them), and each goal's latest check-in. Progress is worked out from it,
// once per goal.
type board struct {
	reader   Reader
	now      time.Time
	goals    map[uuid.UUID]*goalModel.Goal
	order    []uuid.UUID
	children map[uuid.UUID][]uuid.UUID
	links    map[uuid.UUID][]uuid.UUID
	projects map[string]projectDomain.ProjectProgress
	latest   map[uuid.UUID]goalModel.CheckIn
	progress map[uuid.UUID]*float64
}

// today is midnight of now's day, in its zone.
func today(now time.Time) time.Time {
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location())
}

// load reads the board: the live goals up to MaxGoals, open ones first. now
// is in the reader's zone: overdue tasks and how much of a goal's time has
// passed are counted in their days.
var load = func(ctx context.Context, r Reader, now time.Time) (*board, error) {
	list, err := goalModel.ListLive()
	if err != nil {
		return nil, err
	}
	b := &board{
		reader: r, now: now,
		goals:    make(map[uuid.UUID]*goalModel.Goal, len(list)),
		children: map[uuid.UUID][]uuid.UUID{},
		links:    map[uuid.UUID][]uuid.UUID{},
		projects: map[string]projectDomain.ProjectProgress{},
		latest:   map[uuid.UUID]goalModel.CheckIn{},
		progress: map[uuid.UUID]*float64{},
	}
	return b, b.include(ctx, list)
}

// include puts goals on the board with their projects' counts and latest
// check-ins, and indexes the tree again. Goals already there are skipped.
func (b *board) include(ctx context.Context, list []goalModel.Goal) error {
	var ids []uuid.UUID
	for i := range list {
		if g := &list[i]; b.goals[g.Id] == nil {
			b.goals[g.Id] = g
			b.order = append(b.order, g.Id)
			ids = append(ids, g.Id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	b.children = map[uuid.UUID][]uuid.UUID{}
	for _, id := range b.order {
		if p := b.goals[id].ParentId; p != nil && b.goals[*p] != nil {
			b.children[*p] = append(b.children[*p], id)
		}
	}
	links, err := goalModel.Links(ids)
	if err != nil {
		return err
	}
	var missing []uuid.UUID
	for id, ps := range links {
		b.links[id] = ps
		for _, p := range ps {
			if _, ok := b.projects[p.String()]; !ok {
				missing = append(missing, p)
			}
		}
	}
	got, err := projectDomain.GetDgraphProjectsProgress(ctx, missing, b.reader.DgraphUID, today(b.now))
	if err != nil {
		return err
	}
	for k, v := range got {
		b.projects[k] = v
	}
	// Without check-ins the goals still show, with no health.
	if latest, err := goalModel.LatestCheckIns(ids); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Goal/include check-ins err: %+v", err)
	} else {
		for k, v := range latest {
			b.latest[k] = v
		}
	}
	b.progress = map[uuid.UUID]*float64{}
	return nil
}

// ensure puts a goal and its sub-goals on the board whatever the first read
// left out, so a goal's page never depends on how many goals there are.
func (b *board) ensure(ctx context.Context, id uuid.UUID) error {
	var extra []goalModel.Goal
	if b.goals[id] == nil {
		g, err := goalModel.Get(id)
		if err != nil {
			return err
		}
		if g == nil {
			return ErrNotFound
		}
		extra = append(extra, *g)
	}
	kids, err := goalModel.Children(id)
	if err != nil {
		return err
	}
	return b.include(ctx, append(extra, kids...))
}

// progressOf is a goal's progress, 0 to 1, or nil when there is nothing to
// measure yet (no project with tasks, no sub-goal with progress). A closed
// goal keeps the progress it closed at. Sub-goals are followed at most
// MaxDepth deep, never round a loop, and a dropped one doesn't count.
func (b *board) progressOf(id uuid.UUID) *float64 {
	return b.progressAt(id, 0, map[uuid.UUID]bool{})
}

func (b *board) progressAt(id uuid.UUID, depth int, visiting map[uuid.UUID]bool) *float64 {
	if p, ok := b.progress[id]; ok {
		return p
	}
	g := b.goals[id]
	if g == nil || visiting[id] || depth > MaxDepth {
		return nil
	}
	visiting[id] = true
	defer delete(visiting, id)
	var p *float64
	switch {
	case g.Status != goalModel.StatusOpen:
		// Closed: where it ended, even if that was nothing to measure.
		p = g.FinalProgress
	case g.Measure == goalModel.MeasureNumber:
		if g.StartValue != nil && g.TargetValue != nil && g.CurrentValue != nil {
			v := NumberProgress(*g.StartValue, *g.TargetValue, *g.CurrentValue)
			p = &v
		}
	case g.Measure == goalModel.MeasureProjects:
		var vs []float64
		for _, pid := range b.links[id] {
			pp, ok := b.projects[pid.String()]
			if !ok || pp.Archived() {
				continue
			}
			if v := TaskProgress(pp.Open, pp.Done); v != nil {
				vs = append(vs, *v)
			}
		}
		p = mean(vs)
	case g.Measure == goalModel.MeasureSubgoals:
		var vs []float64
		for _, c := range b.children[id] {
			if b.goals[c].Status == goalModel.StatusDropped {
				continue
			}
			if v := b.progressAt(c, depth+1, visiting); v != nil {
				vs = append(vs, *v)
			}
		}
		p = mean(vs)
	}
	b.progress[id] = p
	return p
}

// Summary is a goal as the list shows it.
type Summary struct {
	Id           string                 `json:"id"`
	Title        string                 `json:"title"`
	Owner        userDomain.UserDisplay `json:"owner"`
	StartDate    string                 `json:"start_date,omitempty"`
	DueDate      string                 `json:"due_date"`
	Measure      string                 `json:"measure"`
	StartValue   *float64               `json:"start_value,omitempty"`
	TargetValue  *float64               `json:"target_value,omitempty"`
	CurrentValue *float64               `json:"current_value,omitempty"`
	Unit         string                 `json:"unit,omitempty"`
	// Progress is 0 to 1; null when there is nothing to measure yet.
	Progress *float64 `json:"progress"`
	// Expected is where an open goal would be by now if it moved evenly.
	Expected *float64 `json:"expected,omitempty"`
	// Health is the latest check-in's; empty before the first.
	Health      string     `json:"health,omitempty"`
	CheckedInAt *time.Time `json:"checked_in_at,omitempty"`
	Status      string     `json:"status"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
	ParentId    string     `json:"parent_id,omitempty"`
	Projects    int        `json:"projects"`
	Subgoals    int        `json:"subgoals"`
	CanEdit     bool       `json:"can_edit"`
	CreatedAt   time.Time  `json:"created_at"`
}

func (b *board) summary(g *goalModel.Goal, people map[string]userDomain.UserDisplay) Summary {
	s := Summary{
		Id: g.Id.String(), Title: g.Title, DueDate: g.DueDate.Format(goalModel.DateLayout), Measure: g.Measure,
		StartValue: g.StartValue, TargetValue: g.TargetValue, CurrentValue: g.CurrentValue, Unit: g.Unit,
		Progress: b.progressOf(g.Id), Status: g.Status, ClosedAt: g.ClosedAt,
		Projects: len(b.links[g.Id]), Subgoals: len(b.children[g.Id]), CanEdit: b.reader.CanEdit(g), CreatedAt: g.CreatedAt,
	}
	s.Owner = people[g.OwnerUUID.String()]
	if s.Owner.Uuid == "" {
		s.Owner.Uuid = g.OwnerUUID.String()
	}
	if g.StartDate != nil {
		s.StartDate = g.StartDate.Format(goalModel.DateLayout)
	}
	if g.ParentId != nil {
		s.ParentId = g.ParentId.String()
	}
	if g.Status == goalModel.StatusOpen {
		e := Expected(startOf(g, b.now.Location()), g.DueDate, b.now)
		s.Expected = &e
	}
	// A reopened goal's last check-in is the one that closed it: that says
	// how it ended, not where it stands, so it waits for a new one.
	if c, ok := b.latest[g.Id]; ok && !(g.Status == goalModel.StatusOpen && Closing(c.Health)) {
		s.Health, s.CheckedInAt = c.Health, &c.CreatedAt
	}
	return s
}

// people is the display of each goal's owner, in one read.
func people(ctx context.Context, goals []*goalModel.Goal, more ...uuid.UUID) map[string]userDomain.UserDisplay {
	ids := make([]string, 0, len(goals)+len(more))
	for _, g := range goals {
		ids = append(ids, g.OwnerUUID.String())
	}
	for _, id := range more {
		ids = append(ids, id.String())
	}
	out, err := userDomain.ResolveUserDisplays(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Goal/people err: %+v", err)
	}
	return out
}

// sortGoals orders goals as lists show them: open first, soonest due first,
// then by title; closed ones most recently closed first.
func sortGoals(list []Summary) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if (a.Status == goalModel.StatusOpen) != (b.Status == goalModel.StatusOpen) {
			return a.Status == goalModel.StatusOpen
		}
		if a.Status != goalModel.StatusOpen && a.ClosedAt != nil && b.ClosedAt != nil && !a.ClosedAt.Equal(*b.ClosedAt) {
			return a.ClosedAt.After(*b.ClosedAt)
		}
		if a.DueDate != b.DueDate {
			return a.DueDate < b.DueDate
		}
		return strings.ToLower(a.Title) < strings.ToLower(b.Title)
	})
}

// List is every live goal, open and closed, as the reader sees them.
func List(ctx context.Context, r Reader, now time.Time) ([]Summary, error) {
	b, err := load(ctx, r, now)
	if err != nil {
		return nil, err
	}
	goals := make([]*goalModel.Goal, 0, len(b.order))
	for _, id := range b.order {
		goals = append(goals, b.goals[id])
	}
	who := people(ctx, goals)
	out := make([]Summary, 0, len(goals))
	for _, g := range goals {
		out = append(out, b.summary(g, who))
	}
	sortGoals(out)
	return out, nil
}

// Ref names another goal.
type Ref struct {
	Id    string `json:"id"`
	Title string `json:"title"`
}

// ProjectLine is a project serving a goal, as the goal shows it: how much of
// its work is done, what is late, and where its people last said it stood.
type ProjectLine struct {
	UUID      string     `json:"project_uuid"`
	Name      string     `json:"project_name"`
	Open      int        `json:"open"`
	Done      int        `json:"done"`
	Overdue   int        `json:"overdue"`
	Progress  *float64   `json:"progress"`
	Health    string     `json:"health,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	Archived  bool       `json:"archived,omitempty"`
}

// CheckInView is a check-in as people read it: who wrote it, by name.
type CheckInView struct {
	goalModel.CheckIn
	AuthorName string `json:"author_name"`
}

// Detail is one goal with everything its page shows.
type Detail struct {
	Summary
	Description string `json:"description"`
	Parent      *Ref   `json:"parent,omitempty"`
	// ProjectList is the projects serving it that the reader is in;
	// HiddenProjects counts the rest, which still count towards progress.
	ProjectList    []ProjectLine `json:"project_list"`
	HiddenProjects int           `json:"hidden_projects"`
	SubgoalList    []Summary     `json:"subgoal_list"`
	CheckIns       []CheckInView `json:"checkins"`
}

// lines is the goal's projects the reader is in, and how many they aren't in.
// A project gone from the graph is left out of both.
func (b *board) lines(id uuid.UUID) ([]ProjectLine, int) {
	shown := []ProjectLine{}
	hidden := 0
	for _, pid := range b.links[id] {
		pp, ok := b.projects[pid.String()]
		if !ok {
			continue
		}
		if !pp.Visible() {
			// An archived project counts for nothing, so it isn't one that "counts too".
			if !pp.Archived() {
				hidden++
			}
			continue
		}
		shown = append(shown, ProjectLine{
			UUID: pp.UUID, Name: pp.Name, Open: pp.Open, Done: pp.Done, Overdue: pp.Overdue,
			Progress: TaskProgress(pp.Open, pp.Done), Archived: pp.Archived(),
		})
	}
	return shown, hidden
}

// projectLines is lines with where each project's latest update said it stood.
func (b *board) projectLines(ctx context.Context, id uuid.UUID) ([]ProjectLine, int) {
	shown, hidden := b.lines(id)
	ids := make([]uuid.UUID, 0, len(shown))
	for _, l := range shown {
		if pid, err := uuid.Parse(l.UUID); err == nil {
			ids = append(ids, pid)
		}
	}
	if len(ids) == 0 {
		return shown, hidden
	}
	latest, err := updateModel.Latest(ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Goal/projectLines updates err: %+v", err)
	}
	for i := range shown {
		if pid, err := uuid.Parse(shown[i].UUID); err == nil {
			if u, ok := latest[pid]; ok {
				shown[i].Health, shown[i].UpdatedAt = u.Health, &u.CreatedAt
			}
		}
	}
	return shown, hidden
}

// checkInViews names each check-in's author.
func checkInViews(ctx context.Context, list []goalModel.CheckIn) []CheckInView {
	ids := make([]string, 0, len(list))
	for _, c := range list {
		ids = append(ids, c.AuthorUUID.String())
	}
	names, err := userDomain.ResolveUserDisplays(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Goal/checkInViews names err: %+v", err)
	}
	out := make([]CheckInView, 0, len(list))
	for _, c := range list {
		v := CheckInView{CheckIn: c, AuthorName: "Someone"}
		if p, ok := names[c.AuthorUUID.String()]; ok {
			if p.FullName != "" {
				v.AuthorName = p.FullName
			} else if p.Name != "" {
				v.AuthorName = p.Name
			}
		}
		out = append(out, v)
	}
	return out
}

// Get is one goal with its projects, sub-goals and check-ins.
func Get(ctx context.Context, r Reader, id uuid.UUID, now time.Time) (*Detail, error) {
	b, err := load(ctx, r, now)
	if err != nil {
		return nil, err
	}
	if err := b.ensure(ctx, id); err != nil {
		return nil, err
	}
	return b.detail(ctx, id)
}

func (b *board) detail(ctx context.Context, id uuid.UUID) (*Detail, error) {
	g := b.goals[id]
	if g == nil {
		return nil, ErrNotFound
	}
	subs := make([]*goalModel.Goal, 0, len(b.children[id]))
	for _, c := range b.children[id] {
		subs = append(subs, b.goals[c])
	}
	who := people(ctx, append(subs, g))
	d := &Detail{Summary: b.summary(g, who), Description: g.Description, SubgoalList: make([]Summary, 0, len(subs))}
	if g.ParentId != nil {
		if p := b.goals[*g.ParentId]; p != nil {
			d.Parent = &Ref{Id: p.Id.String(), Title: p.Title}
		}
	}
	for _, s := range subs {
		d.SubgoalList = append(d.SubgoalList, b.summary(s, who))
	}
	sortGoals(d.SubgoalList)
	d.ProjectList, d.HiddenProjects = b.projectLines(ctx, id)
	list, err := goalModel.CheckIns(id, goalModel.MaxCheckIns)
	if err != nil {
		return nil, err
	}
	d.CheckIns = checkInViews(ctx, list)
	return d, nil
}

// ForProject is the open goals a project serves, as its page names them.
func ForProject(ctx context.Context, r Reader, projectID uuid.UUID, now time.Time) ([]Summary, error) {
	b, err := load(ctx, r, now)
	if err != nil {
		return nil, err
	}
	var goals []*goalModel.Goal
	for _, id := range b.order {
		if g := b.goals[id]; g.Status == goalModel.StatusOpen {
			for _, p := range b.links[id] {
				if p == projectID {
					goals = append(goals, g)
					break
				}
			}
		}
	}
	who := people(ctx, goals)
	out := make([]Summary, 0, len(goals))
	for _, g := range goals {
		out = append(out, b.summary(g, who))
	}
	sortGoals(out)
	return out, nil
}
