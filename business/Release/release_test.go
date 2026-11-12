package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// world is one project with one release whose tasks are open (a), done (b),
// restricted from the reader (c: the graph read doesn't answer it), deleted
// (d) and moved to another project (e). more adds releases after it.
type world struct {
	project, other uuid.UUID
	rel            releaseModel.Release
	rels           []releaseModel.Release // the project's releases, in list order
	in             map[uuid.UUID][]uuid.UUID
	a, b, c, d, e  uuid.UUID
	role           workAccess.Role
	// archived is the project's archive alone; gone is a project that
	// isn't there any more.
	archived, gone bool
	readAs         []string // the reader of each task read
	read           [][]string
	links, stages  [][]uuid.UUID // the releases each read of their tasks, and of their stages, took
	listed         []int         // the most each list read asked for
	added          []uuid.UUID
	dropped        []uuid.UUID
	full           bool
}

// more adds a release after the others, with tasks.
func (w *world) more(name string, tasks ...uuid.UUID) releaseModel.Release {
	r := releaseModel.Release{ID: uuid.New(), ProjectID: w.project, Name: name, State: releaseModel.Planned}
	w.rels = append(w.rels, r)
	w.in[r.ID] = tasks
	return r
}

func newWorld(t *testing.T, role workAccess.Role) *world {
	t.Helper()
	w := &world{project: uuid.New(), other: uuid.New(), role: role, in: map[uuid.UUID][]uuid.UUID{},
		a: uuid.New(), b: uuid.New(), c: uuid.New(), d: uuid.New(), e: uuid.New()}
	w.rel = w.more("v1.0", w.a, w.b, w.c, w.d, w.e)
	was := []any{projectFor, standingOf, readTasks, layKeys, listRows, getRow, membersOf, stagesOf, addRows, dropRow}
	t.Cleanup(func() {
		projectFor = was[0].(func(context.Context, string, string) (*dgraphStruct.DgraphProject, error))
		standingOf = was[1].(func(context.Context, string, *dgraphStruct.DgraphProject) (workAccess.Standing, error))
		readTasks = was[2].(func(context.Context, []string, string) ([]*dgraphStruct.DgraphTask, error))
		layKeys = was[3].(func(context.Context, ...*dgraphStruct.DgraphTask))
		listRows = was[4].(func(context.Context, uuid.UUID, uuid.UUID, int) ([]releaseModel.Release, error))
		getRow = was[5].(func(context.Context, uuid.UUID) (releaseModel.Release, bool, error))
		membersOf = was[6].(func(context.Context, []uuid.UUID) (map[uuid.UUID][]releaseModel.Member, error))
		stagesOf = was[7].(func(context.Context, []uuid.UUID) (map[uuid.UUID][]releaseModel.Stage, error))
		addRows = was[8].(func(context.Context, uuid.UUID, []uuid.UUID, string, uuid.UUID, int) ([]string, error))
		dropRow = was[9].(func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error))
	})
	projectFor = func(_ context.Context, id, _ string) (*dgraphStruct.DgraphProject, error) {
		if id != w.project.String() || w.gone {
			return nil, nil
		}
		return &dgraphStruct.DgraphProject{Uuid: id}, nil
	}
	standingOf = func(_ context.Context, _ string, p *dgraphStruct.DgraphProject) (workAccess.Standing, error) {
		return workAccess.Standing{ProjectUUID: p.Uuid, Role: w.role, Archived: w.archived}, nil
	}
	gone := time.Now()
	task := func(id uuid.UUID, status string, project uuid.UUID) *dgraphStruct.DgraphTask {
		return &dgraphStruct.DgraphTask{Uuid: id.String(), Name: "task " + id.String()[:4], Status: status,
			Project: &dgraphStruct.DgraphProject{Uuid: strings.ToUpper(project.String())}}
	}
	readTasks = func(ctx context.Context, ids []string, _ string) ([]*dgraphStruct.DgraphTask, error) {
		w.readAs = append(w.readAs, taskread.ReaderOf(ctx))
		w.read = append(w.read, ids)
		deleted := task(w.d, dgraphStruct.TASK_STATUS_TODO, w.project)
		deleted.DeletedAt = &gone
		all := map[string]*dgraphStruct.DgraphTask{
			w.a.String(): task(w.a, dgraphStruct.TASK_STATUS_TODO, w.project),
			w.b.String(): task(w.b, dgraphStruct.TASK_STATUS_DONE, w.project),
			w.d.String(): deleted,
			w.e.String(): task(w.e, dgraphStruct.TASK_STATUS_DONE, w.other),
		}
		var out []*dgraphStruct.DgraphTask
		for _, id := range ids {
			if t := all[id]; t != nil {
				out = append(out, t)
			}
		}
		return out, nil
	}
	layKeys = func(_ context.Context, ts ...*dgraphStruct.DgraphTask) {
		for _, t := range ts {
			t.Key = "WEB-" + t.Uuid[:2]
		}
	}
	// As the model's keyset: the releases after the one named, limit of them
	// (all for none), and none after a release the project hasn't.
	listRows = func(_ context.Context, _ uuid.UUID, after uuid.UUID, limit int) ([]releaseModel.Release, error) {
		w.listed = append(w.listed, limit)
		from := 0
		if after != uuid.Nil {
			from = len(w.rels)
			for i, r := range w.rels {
				if r.ID == after {
					from = i + 1
				}
			}
		}
		out := w.rels[from:]
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return out, nil
	}
	getRow = func(_ context.Context, id uuid.UUID) (releaseModel.Release, bool, error) {
		for _, r := range w.rels {
			if r.ID == id {
				return r, true, nil
			}
		}
		return releaseModel.Release{}, false, nil
	}
	membersOf = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID][]releaseModel.Member, error) {
		w.links = append(w.links, ids)
		out := map[uuid.UUID][]releaseModel.Member{}
		for _, id := range ids {
			for _, task := range w.in[id] {
				out[id] = append(out[id], releaseModel.Member{Task: task, Source: releaseModel.Manual})
			}
		}
		return out, nil
	}
	stagesOf = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID][]releaseModel.Stage, error) {
		w.stages = append(w.stages, ids)
		return map[uuid.UUID][]releaseModel.Stage{}, nil
	}
	addRows = func(_ context.Context, _ uuid.UUID, tasks []uuid.UUID, source string, _ uuid.UUID, most int) ([]string, error) {
		if w.full {
			return nil, releaseModel.ErrFull
		}
		if source != releaseModel.Manual || most != MostTasks {
			t.Errorf("added as %q with most %d", source, most)
		}
		w.added = tasks
		var out []string
		for _, id := range tasks {
			if id != w.a { // a is in already
				out = append(out, id.String())
			}
		}
		return out, nil
	}
	dropRow = func(_ context.Context, release, task, by uuid.UUID) (bool, error) {
		if by.String() != ben.UserUUID {
			t.Errorf("taken out by %s, want the remover", by)
		}
		w.dropped = append(w.dropped, task)
		for _, id := range w.in[release] {
			if id == task {
				return true, nil
			}
		}
		return false, nil
	}
	return w
}

var ben = accessBusiness.Subject{UserUID: "0x2", UserUUID: uuid.NewString()}

// A member reads a release counting only the tasks they may see: a
// restricted task kept from them, a deleted one and one moved to another
// project count for nothing. They may not change it; an admin may.
func TestAMemberReadsOnlyTheTasksTheyMaySee(t *testing.T) {
	w := newWorld(t, workAccess.RoleMember)
	list, err := Releases.List(context.Background(), ben, w.project, "", 0)
	if err != nil || len(list.Releases) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if got := list.Releases[0].Progress; got.Total != 2 || got.Done != 1 {
		t.Errorf("progress %+v, want 1 of 2 done", got)
	}
	if list.CanEdit || list.Releases[0].CanEdit {
		t.Errorf("a member may change releases: %+v", list)
	}
	rel, err := Releases.Get(context.Background(), ben, w.rel.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range rel.Tasks {
		got = append(got, x.UUID+" "+x.Key)
	}
	want := []string{w.a.String() + " WEB-" + w.a.String()[:2], w.b.String() + " WEB-" + w.b.String()[:2]}
	if strings.Join(got, ",") != strings.Join(want, ",") || rel.CanEdit {
		t.Errorf("tasks %v can_edit %v, want %v", got, rel.CanEdit, want)
	}
	for _, r := range w.readAs {
		if r != ben.UserUUID {
			t.Errorf("tasks read as %q, want the reader %q", r, ben.UserUUID)
		}
	}
	admin := newWorld(t, workAccess.RoleAdmin)
	if list, err := Releases.List(context.Background(), ben, admin.project, "", 0); err != nil || !list.CanEdit {
		t.Errorf("an admin's list: %+v %v", list, err)
	}
}

// Someone the project isn't there for, and anyone once it is gone, finds no
// releases: not found, as if there were none.
func TestOutsideALiveProjectThereAreNoReleases(t *testing.T) {
	w := newWorld(t, workAccess.RoleNone)
	if _, err := Releases.List(context.Background(), ben, w.project, "", 0); !errors.Is(err, helpers.ErrNotFound) {
		t.Errorf("an outsider's list: %v", err)
	}
	if _, err := Releases.Get(context.Background(), ben, w.rel.ID); !errors.Is(err, helpers.ErrNotFound) {
		t.Errorf("an outsider's read: %v", err)
	}
	w.role, w.gone = workAccess.RoleAdmin, true
	if _, err := Releases.List(context.Background(), ben, w.project, "", 0); !errors.Is(err, helpers.ErrNotFound) {
		t.Errorf("a deleted project's list: %v", err)
	}
	if _, err := Releases.Get(context.Background(), ben, w.rel.ID); !errors.Is(err, helpers.ErrNotFound) {
		t.Errorf("a deleted project's release: %v", err)
	}
}

// An archived project's releases stay readable, as its milestones and cycles
// do, and read-only: even its admins may change none of them, until it is
// restored.
func TestAnArchivedProjectsReleasesAreReadOnly(t *testing.T) {
	w := newWorld(t, workAccess.RoleAdmin)
	w.archived = true
	list, err := Releases.List(context.Background(), ben, w.project, "", 0)
	if err != nil || len(list.Releases) != 1 || list.Releases[0].Progress.Total != 2 {
		t.Fatalf("an archived project's list: %+v %v", list, err)
	}
	if list.CanEdit || list.Releases[0].CanEdit {
		t.Errorf("an archived project's releases can be changed: %+v", list)
	}
	rel, err := Releases.Get(context.Background(), ben, w.rel.ID)
	if err != nil || len(rel.Tasks) != 2 || rel.CanEdit {
		t.Errorf("an archived project's release: %+v %v", rel, err)
	}
	for _, x := range []struct {
		role     workAccess.Role
		archived bool
		edit     bool
	}{
		{workAccess.RoleAdmin, false, true},
		{workAccess.RoleAdmin, true, false},
		{workAccess.RoleMember, false, false},
	} {
		if got := Editable(workAccess.Standing{Role: x.role, Archived: x.archived}); got != x.edit {
			t.Errorf("%s, archived %v, may change releases: %v, want %v", x.role, x.archived, got, x.edit)
		}
	}
}

// A list reads a page at a time: limit releases (PageSize unasked, PageMost
// at most), after the page its cursor ends, and the tasks and stages of that
// page's releases only, in one read of each.
func TestAListReadsAPageAtATime(t *testing.T) {
	w := newWorld(t, workAccess.RoleMember)
	second, third := uuid.New(), uuid.New()
	r2, r3 := w.more("v1.1", second), w.more("v1.2", third)
	ids := func(rs []Release) (out []uuid.UUID) {
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return
	}
	same := func(what string, got []uuid.UUID, want ...uuid.UUID) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: %v, want %v", what, got, want)
		}
	}
	page, err := Releases.List(context.Background(), ben, w.project, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	same("the first page", ids(page.Releases), w.rel.ID, r2.ID)
	if page.Next != r2.ID.String() {
		t.Errorf("the first page's cursor %q, want its last release %s", page.Next, r2.ID)
	}
	if len(w.links) != 1 || len(w.stages) != 1 || len(w.read) != 1 {
		t.Fatalf("one read each of tasks, stages and the graph: %v %v %v", w.links, w.stages, w.read)
	}
	same("the first page's tasks read", w.links[0], w.rel.ID, r2.ID)
	same("the first page's stages read", w.stages[0], w.rel.ID, r2.ID)
	if read := strings.Join(w.read[0], ","); !strings.Contains(read, second.String()) || strings.Contains(read, third.String()) {
		t.Errorf("the first page's graph read %v, want its tasks only", w.read[0])
	}
	w.links, w.stages, w.read = nil, nil, nil
	page, err = Releases.List(context.Background(), ben, w.project, page.Next, 2)
	if err != nil {
		t.Fatal(err)
	}
	same("the last page", ids(page.Releases), r3.ID)
	if page.Next != "" || len(w.links) != 1 || len(w.read) != 1 {
		t.Fatalf("the last page: cursor %q, reads %v %v", page.Next, w.links, w.read)
	}
	same("the last page's tasks read", w.links[0], r3.ID)
	if fmt.Sprint(w.read[0]) != fmt.Sprint([]string{third.String()}) {
		t.Errorf("the last page's graph read %v, want %s alone", w.read[0], third)
	}
	w.listed = nil
	for _, limit := range []int{0, -1, PageMost + 1, PageMost} {
		if _, err := Releases.List(context.Background(), ben, w.project, "", limit); err != nil {
			t.Fatal(err)
		}
	}
	// One more than a page is read, to know whether there's a next.
	if fmt.Sprint(w.listed) != fmt.Sprint([]int{PageSize + 1, PageSize + 1, PageMost + 1, PageMost + 1}) {
		t.Errorf("read %v at a time, want a page of %d unasked and %d at most", w.listed, PageSize, PageMost)
	}
	var re *ReleaseError
	if _, err := Releases.List(context.Background(), ben, w.project, "WEB-1", 2); !errors.As(err, &re) {
		t.Errorf("a cursor no list gave: %v, want words", err)
	}
	outsider := newWorld(t, workAccess.RoleNone)
	if _, err := Releases.List(context.Background(), ben, outsider.project, "WEB-1", 2); !errors.Is(err, helpers.ErrNotFound) {
		t.Errorf("an outsider's bad cursor: %v, want not found", err)
	}
}

// Taking a task out reads it as the remover first, as putting one in does: a
// task they may not see now (kept from them, deleted, another project's) is
// not found, as one not in the release is, and nothing is written.
func TestRemoveTaskTakesOutOnlyATaskTheRemoverSees(t *testing.T) {
	w := newWorld(t, workAccess.RoleAdmin)
	for _, id := range []uuid.UUID{w.c, w.d, w.e} {
		if err := RemoveTask(context.Background(), ben, w.rel, id); !errors.Is(err, helpers.ErrNotFound) {
			t.Errorf("%s: %v, want not found", id, err)
		}
	}
	if len(w.dropped) != 0 {
		t.Errorf("took out %v, which the remover may not see", w.dropped)
	}
	if err := RemoveTask(context.Background(), ben, w.rel, w.a); err != nil || fmt.Sprint(w.dropped) != fmt.Sprint([]uuid.UUID{w.a}) {
		t.Errorf("a task the remover sees: %v, took out %v", err, w.dropped)
	}
	if len(w.readAs) != 4 {
		t.Errorf("%d reads, want each task read", len(w.readAs))
	}
	for _, r := range w.readAs {
		if r != ben.UserUUID {
			t.Errorf("read as %q, want the remover %q", r, ben.UserUUID)
		}
	}
}

// Every task a request names is read and checked: only a live task of the
// release's project its adder may see goes in, and the refusal never says
// which of the other cases it was.
func TestAddTasksTakesOnlyTheProjectsLiveTasksTheAdderSees(t *testing.T) {
	w := newWorld(t, workAccess.RoleAdmin)
	got, err := AddTasks(context.Background(), ben, w.rel,
		[]string{w.a.String(), w.b.String(), strings.ToUpper(w.b.String()), w.c.String(), w.d.String(), w.e.String(), "WEB-1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Added, ",") != w.b.String() || strings.Join(got.Already, ",") != w.a.String() {
		t.Errorf("added %v already %v", got.Added, got.Already)
	}
	refused := map[string]string{}
	for _, r := range got.Refused {
		refused[r.Task] = r.Msg
	}
	for _, id := range []uuid.UUID{w.c, w.d, w.e} {
		if refused[id.String()] != notSeen {
			t.Errorf("%s: %q, want %q", id, refused[id.String()], notSeen)
		}
	}
	if refused["WEB-1"] != "That isn't a task." || len(got.Refused) != 4 {
		t.Errorf("refused %+v", got.Refused)
	}
	if len(w.added) != 2 || len(w.readAs) != 1 || w.readAs[0] != ben.UserUUID {
		t.Errorf("wrote %v, read as %v", w.added, w.readAs)
	}
	w.full = true
	var re *ReleaseError
	if _, err := AddTasks(context.Background(), ben, w.rel, []string{w.b.String()}); !errors.As(err, &re) {
		t.Errorf("a full release: %v", err)
	}
	// A release deleted since the route read it: not found, never "already".
	w.full = false
	was := addRows
	addRows = func(context.Context, uuid.UUID, []uuid.UUID, string, uuid.UUID, int) ([]string, error) {
		return nil, helpers.ErrNotFound
	}
	if _, err := AddTasks(context.Background(), ben, w.rel, []string{w.b.String()}); !errors.Is(err, helpers.ErrNotFound) {
		t.Errorf("a release deleted meanwhile: %v, want not found", err)
	}
	addRows = was
	many := make([]string, MostAtOnce+1)
	if _, err := AddTasks(context.Background(), ben, w.rel, many); !errors.As(err, &re) {
		t.Errorf("too many at once: %v", err)
	}
}

func TestInputIsCleanedAndBounded(t *testing.T) {
	day, bad := "2026-11-20", "20 Nov"
	in, err := Input{Name: "  v2.1\n  beta ", TargetDate: &day, Notes: " notes "}.Clean()
	if err != nil || in.Name != "v2.1 beta" || in.Notes != "notes" || in.TargetDate == nil || *in.TargetDate != day {
		t.Errorf("%+v %v", in, err)
	}
	for _, x := range []Input{
		{Name: "  "},
		{Name: strings.Repeat("é", nameMost+1)},
		{Name: "v1", Notes: strings.Repeat("n", notesMost+1)},
		{Name: "v1\x00"},
		{Name: "v1", TargetDate: &bad},
	} {
		var re *ReleaseError
		if _, err := x.Clean(); !errors.As(err, &re) {
			t.Errorf("%.40q: %v, want words", x.Name, err)
		}
	}
	if in, err := (Input{Name: strings.Repeat("é", nameMost)}).Clean(); err != nil || in.TargetDate != nil {
		t.Errorf("a name at its most: %v", err)
	}
	empty, notes := "", "new"
	got := Edit{TargetDate: &empty, Notes: &notes}.Apply(releaseModel.Release{Name: "v1", TargetDate: &day, Notes: "old"})
	if got.Name != "v1" || got.Notes != "new" || got.TargetDate == nil || *got.TargetDate != "" {
		t.Errorf("an edit keeps what it doesn't set: %+v", got)
	}
}
