package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	cycleBusiness "github.com/akashc777/OneCamp/business/Cycle"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

const (
	// nameMost and notesMost are migration 580's bounds.
	nameMost  = 120
	notesMost = 20000
	// MostTasks is the most tasks one release holds, so a project's list
	// reads its releases' tasks in one bounded read.
	MostTasks = 500
	// MostAtOnce is the most tasks one request puts in.
	MostAtOnce = 100
	// PageSize is how many releases a list reads when it isn't told, and
	// PageMost the most it reads at once.
	PageSize = 50
	PageMost = 100
)

// ReleaseError is a request the person can fix; its text is written for them.
type ReleaseError struct{ msg string }

func (e *ReleaseError) Error() string { return e.msg }

// Seams for tests: the reads a reader's view of releases makes.
var (
	projectFor = projectDomain.GetBasicDgraphProjectInfo
	standingOf = workAccess.Of
	// readTasks reads tasks by uuid for the reader of ctx: one they may not
	// see (restricted, models/taskread) isn't answered.
	readTasks = taskDomain.GetDgraphTasksForWrite
	layKeys   = taskKeys.Lay
	listRows  = releaseModel.List
	getRow    = releaseModel.Get
	membersOf = releaseModel.MembersOf
	stagesOf  = releaseModel.StagesOf
	addRows   = releaseModel.AddTasks
	dropRow   = releaseModel.RemoveTask
	shipRows  = releaseModel.ShipMoving
)

type book struct{}

// view is how who stands in the project, when it is there and theirs to
// view; any other is helpers.ErrNotFound. An archived project's releases are
// viewed as before (as its milestones and cycles are), and Editable says
// none of them may change. Its archive is its DeletedAt (workAccess's
// Archived): a deleted project is one that isn't there.
func view(ctx context.Context, who accessBusiness.Subject, project uuid.UUID) (workAccess.Standing, error) {
	p, err := projectFor(ctx, project.String(), who.UserUID)
	if err == nil && (p == nil || p.Uuid == "") {
		err = helpers.ErrNotFound
	}
	if err != nil {
		return workAccess.Standing{}, err
	}
	s, err := standingOf(ctx, who.UserUUID, p)
	if err != nil {
		return s, err
	}
	if !s.Can(workAccess.NeedView) {
		return s, helpers.ErrNotFound
	}
	return s, nil
}

// Editable is whether standing s may change a project's releases: its
// admins, while it isn't archived. The routes refuse a change by it, and a
// read answers it as can_edit. Pure.
func Editable(s workAccess.Standing) bool {
	return !s.Archived && s.Can(workAccess.NeedManage)
}

// seen is, of ids, the tasks of project the reader of ctx (by graph uid) may
// see now, by lower-case uuid: live, still in the project, and not kept from
// them. One graph read however many.
func seen(ctx context.Context, project uuid.UUID, ids []string, readerUID string) (map[string]*dgraphStruct.DgraphTask, error) {
	out := make(map[string]*dgraphStruct.DgraphTask, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	tasks, err := readTasks(ctx, ids, readerUID)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if inProject(t, project) {
			out[strings.ToLower(t.Uuid)] = t
		}
	}
	return out, nil
}

// inProject is whether t is a live task of project its reader was answered.
// Pure.
func inProject(t *dgraphStruct.DgraphTask, project uuid.UUID) bool {
	return t != nil && t.Uuid != "" && !t.Hidden && !helpers.IsSoftDeleted(t.DeletedAt) &&
		t.Project != nil && strings.EqualFold(t.Project.Uuid, project.String())
}

// progress counts the members the reader sees (seen) by where they stand.
// Pure.
func progress(members []releaseModel.Member, seen map[string]*dgraphStruct.DgraphTask) cycleBusiness.Progress {
	var statuses []string
	for _, m := range members {
		if t, ok := seen[m.Task.String()]; ok {
			statuses = append(statuses, t.Status)
		}
	}
	return cycleBusiness.Count(statuses)
}

// List is a page of a project's releases as who sees them: each one's
// progress over the tasks they may see, and its stages. The page's releases
// alone are read for their tasks, in one read, and those tasks in one graph
// read, however long the list.
func (book) List(ctx context.Context, who accessBusiness.Subject, project uuid.UUID, cursor string, limit int) (Listing, error) {
	ctx = taskread.ActingFor(ctx, who.UserUUID)
	s, err := view(ctx, who, project)
	if err != nil {
		return Listing{}, err
	}
	var after uuid.UUID
	if cursor != "" {
		if after, err = uuid.Parse(cursor); err != nil {
			return Listing{}, &ReleaseError{"That page cursor isn't one this list gave."}
		}
	}
	if limit < 1 {
		limit = PageSize
	}
	limit = min(limit, PageMost)
	// One past the page says whether another follows.
	rows, err := listRows(ctx, project, after, limit+1)
	if err != nil {
		return Listing{}, err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[limit-1].ID.String()
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	members, stages, tasks, err := around(ctx, project, ids, who.UserUID)
	if err != nil {
		return Listing{}, err
	}
	out := Listing{Releases: make([]Release, 0, len(rows)), CanEdit: Editable(s), Next: next}
	for _, r := range rows {
		out.Releases = append(out.Releases, Release{Release: r, Progress: progress(members[r.ID], tasks),
			Unfinished: len(unfinished(members[r.ID], tasks)), Stages: nonNil(stages[r.ID]), CanEdit: out.CanEdit})
	}
	return out, nil
}

// around reads what hangs off the releases: their tasks, their stages, and
// the tasks of project among them the reader may see, a read of each.
func around(ctx context.Context, project uuid.UUID, ids []uuid.UUID, readerUID string) (map[uuid.UUID][]releaseModel.Member, map[uuid.UUID][]releaseModel.Stage, map[string]*dgraphStruct.DgraphTask, error) {
	members, err := membersOf(ctx, ids)
	if err != nil {
		return nil, nil, nil, err
	}
	stages, err := stagesOf(ctx, ids)
	if err != nil {
		return nil, nil, nil, err
	}
	var all []string
	known := map[uuid.UUID]bool{}
	for _, ms := range members {
		for _, m := range ms {
			if !known[m.Task] {
				known[m.Task] = true
				all = append(all, m.Task.String())
			}
		}
	}
	tasks, err := seen(ctx, project, all, readerUID)
	return members, stages, tasks, err
}

func nonNil(s []releaseModel.Stage) []releaseModel.Stage {
	if s == nil {
		return []releaseModel.Stage{}
	}
	return s
}

// Get is one release as who sees it, with the tasks they may see.
func (book) Get(ctx context.Context, who accessBusiness.Subject, id uuid.UUID) (*Release, error) {
	ctx = taskread.ActingFor(ctx, who.UserUUID)
	row, found, err := getRow(ctx, id)
	if err == nil && !found {
		err = helpers.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s, err := view(ctx, who, row.ProjectID)
	if err != nil {
		return nil, err
	}
	members, stages, tasks, err := around(ctx, row.ProjectID, []uuid.UUID{id}, who.UserUID)
	if err != nil {
		return nil, err
	}
	out := &Release{Release: row, Progress: progress(members[id], tasks), Unfinished: len(unfinished(members[id], tasks)),
		Stages: nonNil(stages[id]), Tasks: []Task{}, CanEdit: Editable(s)}
	var shown []*dgraphStruct.DgraphTask
	for _, m := range members[id] {
		if t, ok := tasks[m.Task.String()]; ok {
			shown = append(shown, t)
		}
	}
	layKeys(ctx, shown...)
	for _, m := range members[id] {
		if t, ok := tasks[m.Task.String()]; ok {
			out.Tasks = append(out.Tasks, Task{UUID: m.Task.String(), Key: t.Key, Name: t.Name, Status: t.Status, Source: m.Source, AddedAt: m.AddedAt})
		}
	}
	return out, nil
}

// Input is a release's name, target day and notes as a person sets them.
type Input struct {
	Name string `json:"name"`
	// TargetDate is a day, "2006-01-02"; null or "" for none.
	TargetDate *string `json:"target_date"`
	Notes      string  `json:"notes"`
}

// Clean is in as a release keeps it, or why it can't: the name on one line
// and both in bounds, the day a real one. Pure.
func (in Input) Clean() (Input, error) {
	out := Input{Name: strings.Join(strings.Fields(in.Name), " "), Notes: strings.TrimSpace(in.Notes)}
	switch n := utf8.RuneCountInString(out.Name); {
	case n == 0:
		return out, &ReleaseError{"Name the release."}
	case n > nameMost:
		return out, &ReleaseError{fmt.Sprintf("Keep the name to %d characters.", nameMost)}
	}
	if utf8.RuneCountInString(out.Notes) > notesMost {
		return out, &ReleaseError{"Keep the notes to 20,000 characters."}
	}
	if strings.ContainsRune(out.Name+out.Notes, 0) || !utf8.ValidString(out.Name+out.Notes) {
		return out, &ReleaseError{"That text has a character a release can't keep."}
	}
	day, ok := helpers.TargetDay(in.TargetDate)
	if !ok {
		return out, &ReleaseError{"That target date couldn't be read."}
	}
	out.TargetDate = day
	return out, nil
}

// Edit is a change to a release: each field set is changed, the others kept.
type Edit struct {
	Name *string `json:"name"`
	// TargetDate "" takes the target day away.
	TargetDate *string `json:"target_date"`
	Notes      *string `json:"notes"`
}

// Apply is r's words with e's changes made. Pure.
func (e Edit) Apply(r releaseModel.Release) Input {
	in := Input{Name: r.Name, TargetDate: r.TargetDate, Notes: r.Notes}
	if e.Name != nil {
		in.Name = *e.Name
	}
	if e.TargetDate != nil {
		in.TargetDate = e.TargetDate
	}
	if e.Notes != nil {
		in.Notes = *e.Notes
	}
	return in
}

// taken is a name another live release of the project has, in words.
func taken(name string) error {
	return &ReleaseError{fmt.Sprintf("This project already has a release named %q.", name)}
}

// Create makes a planned release in project, by whom.
func Create(ctx context.Context, project uuid.UUID, in Input, by uuid.UUID) (*releaseModel.Release, error) {
	in, err := in.Clean()
	if err != nil {
		return nil, err
	}
	r, err := releaseModel.Create(ctx, project, in.Name, in.TargetDate, in.Notes, by)
	if errors.Is(err, releaseModel.ErrTaken) {
		return nil, taken(in.Name)
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Update makes e's changes to r. A release deleted meanwhile is
// helpers.ErrNotFound.
func Update(ctx context.Context, r releaseModel.Release, e Edit) error {
	in, err := e.Apply(r).Clean()
	if err != nil {
		return err
	}
	ok, err := releaseModel.Update(ctx, r.ID, in.Name, in.TargetDate, in.Notes)
	switch {
	case errors.Is(err, releaseModel.ErrTaken):
		return taken(in.Name)
	case err == nil && !ok:
		return helpers.ErrNotFound
	}
	return err
}

// Shipped is what Mark released did: how many unfinished tasks it moved on
// (into the release asked, or out with "none").
type Shipped struct {
	Moved int `json:"moved"`
}

// unfinished is, of members, those the reader sees (seen) still to do:
// open, as burndown counts it (cycleBusiness.IsOpen). Done and cancelled
// ones aren't. Pure.
func unfinished(members []releaseModel.Member, seen map[string]*dgraphStruct.DgraphTask) []uuid.UUID {
	var out []uuid.UUID
	for _, m := range members {
		if t, ok := seen[m.Task.String()]; ok && cycleBusiness.IsOpen(t.Status) {
			out = append(out, m.Task)
		}
	}
	return out
}

// moveWords is the refusal of a move_to that isn't a planned release of the
// project.
const moveWords = "Move the unfinished tasks to another planned release of this project, or to none."

// Ship marks release r out now, for who, one of its project's admins (the
// route checked). moveTo nil keeps every task in it; NoRelease takes its
// unfinished tasks out; another planned release of r's project (the id is
// the client's, read and checked) takes them in. Unfinished is open, of the
// tasks who sees (an admin sees them all); done and cancelled ones stay. One
// transaction. A release out already keeps when it went, and moves nothing.
func Ship(ctx context.Context, who accessBusiness.Subject, r releaseModel.Release, moveTo *string) (Shipped, error) {
	by, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return Shipped{}, helpers.ErrNotFound
	}
	var to *uuid.UUID
	if moveTo != nil && *moveTo != NoRelease {
		id, err := uuid.Parse(strings.TrimSpace(*moveTo))
		if err != nil {
			return Shipped{}, &ReleaseError{moveWords}
		}
		target, found, err := getRow(ctx, id)
		if err != nil {
			return Shipped{}, err
		}
		if !found || target.ProjectID != r.ProjectID || target.State != releaseModel.Planned || target.ID == r.ID {
			return Shipped{}, &ReleaseError{moveWords}
		}
		to = &target.ID
	}
	var open []uuid.UUID
	if moveTo != nil {
		ctx = taskread.ActingFor(ctx, who.UserUUID)
		members, err := membersOf(ctx, []uuid.UUID{r.ID})
		if err != nil {
			return Shipped{}, err
		}
		ids := make([]string, len(members[r.ID]))
		for i, m := range members[r.ID] {
			ids[i] = m.Task.String()
		}
		tasks, err := seen(ctx, r.ProjectID, ids, who.UserUID)
		if err != nil {
			return Shipped{}, err
		}
		open = unfinished(members[r.ID], tasks)
	}
	moved, err := shipRows(ctx, r.ID, open, to, by, MostTasks)
	switch {
	case errors.Is(err, releaseModel.ErrFull):
		return Shipped{}, &ReleaseError{fmt.Sprintf("A release holds at most %d tasks.", MostTasks)}
	case errors.Is(err, helpers.ErrNotFound):
		return Shipped{}, &ReleaseError{moveWords}
	}
	return Shipped{Moved: moved}, err
}

// Delete deletes a release softly, by whom: its tasks and stages stay for
// its restore (POST /restore/release/{id}).
func Delete(ctx context.Context, id, by uuid.UUID) error {
	_, err := releaseModel.Delete(ctx, id, by)
	return err
}

// Restore brings a deleted release back with its tasks and stages; not while
// a release made since has its name, which the words say.
func Restore(ctx context.Context, id uuid.UUID) error {
	_, err := releaseModel.Restore(ctx, id)
	if errors.Is(err, releaseModel.ErrTaken) {
		return &ReleaseError{"A release made since has its name. Rename that one first to bring this one back."}
	}
	return err
}

// Refusal is a task a request named that didn't go in, and why, in words.
type Refusal struct {
	Task string `json:"task_uuid"`
	Msg  string `json:"msg"`
}

// Added is what AddTasks did: the tasks it put in, those already in, and
// those it refused.
type Added struct {
	Added   []string  `json:"added"`
	Already []string  `json:"already"`
	Refused []Refusal `json:"refused"`
}

// notSeen is the one refusal of a task that isn't there, isn't the
// project's, or is kept from the adder: they learn nothing of which.
const notSeen = "That task isn't one of this project's tasks you can see."

// AddTasks puts tasks (by uuid, as a request names them) in r for who, in one
// read and one write however many. A task goes in only when it is a live
// task of r's project who may see now: the ids are the client's, so each is
// read and checked, never trusted. Past MostTasks nothing goes in.
func AddTasks(ctx context.Context, who accessBusiness.Subject, r releaseModel.Release, raw []string) (Added, error) {
	out := Added{Added: []string{}, Already: []string{}, Refused: []Refusal{}}
	if len(raw) > MostAtOnce {
		return out, &ReleaseError{fmt.Sprintf("Add at most %d tasks at a time.", MostAtOnce)}
	}
	by, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return out, helpers.ErrNotFound
	}
	ctx = taskread.ActingFor(ctx, who.UserUUID)
	var asked []string
	known := map[string]bool{}
	for _, s := range raw {
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil {
			out.Refused = append(out.Refused, Refusal{Task: s, Msg: "That isn't a task."})
			continue
		}
		if k := id.String(); !known[k] {
			known[k] = true
			asked = append(asked, k)
		}
	}
	tasks, err := seen(ctx, r.ProjectID, asked, who.UserUID)
	if err != nil {
		return out, err
	}
	var put []uuid.UUID
	for _, id := range asked {
		if tasks[id] == nil {
			out.Refused = append(out.Refused, Refusal{Task: id, Msg: notSeen})
			continue
		}
		put = append(put, uuid.MustParse(id))
	}
	added, err := addRows(ctx, r.ID, put, releaseModel.Manual, by, MostTasks)
	if errors.Is(err, releaseModel.ErrFull) {
		return out, &ReleaseError{fmt.Sprintf("A release holds at most %d tasks.", MostTasks)}
	}
	if err != nil {
		return out, err
	}
	in := map[string]bool{}
	for _, id := range added {
		in[strings.ToLower(id)] = true
	}
	for _, id := range put {
		if k := id.String(); in[k] {
			out.Added = append(out.Added, k)
		} else {
			out.Already = append(out.Already, k)
		}
	}
	return out, nil
}

// RemoveTask takes a task out of r, as who: softly, so putting it back
// revives its row. The task is read as who first, as AddTasks reads it: one
// that isn't a live task of r's project they may see now is
// helpers.ErrNotFound, as one not in r is, so neither its place in r nor
// anything of it is told, and nothing is written.
func RemoveTask(ctx context.Context, who accessBusiness.Subject, r releaseModel.Release, task uuid.UUID) error {
	by, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return helpers.ErrNotFound
	}
	ctx = taskread.ActingFor(ctx, who.UserUUID)
	tasks, err := seen(ctx, r.ProjectID, []string{task.String()}, who.UserUID)
	if err != nil {
		return err
	}
	if tasks[task.String()] == nil {
		return helpers.ErrNotFound
	}
	ok, err := dropRow(ctx, r.ID, task, by)
	if err == nil && !ok {
		err = helpers.ErrNotFound
	}
	return err
}
