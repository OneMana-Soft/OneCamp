package business

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// world is the repair's seams over a few tasks held in memory, with its
// progress kept as the record keeps it (JSON), across runs.
type world struct {
	tasks    []taskDomain.AssignedTask
	hidden   map[string]map[string]bool
	failing  map[string]bool // tasks whose take-off fails
	noBot    bool
	noAdmins bool
	record   []byte
	told     []notificationBusiness.Notice
	bots     int
	// page is how many tasks a page holds (all of them when 0); reads are
	// the people each read of restricted tasks was for; made are the tasks
	// an import made since the repair's start; events is the order of the
	// page reads ("page"), the waits for imports ("idle") and the reads of
	// what imports made ("made"); idleErr is what the wait answers.
	page    int
	reads   [][]string
	made    map[string]bool
	since   []time.Time
	events  []string
	idleErr error
}

const (
	launch = "aa000000-0000-4000-8000-000000000001"
	admin  = "aa000000-0000-4000-8000-0000000000ad"
	// attic is a project with no admin who can be told; owner is the
	// workspace's admin, told instead.
	attic = "aa000000-0000-4000-8000-000000000002"
	owner = "aa000000-0000-4000-8000-0000000000ae"
)

func (w *world) install(t *testing.T) {
	t.Helper()
	page, hid, bot, un, unTrash, admins, active, everyone, told, load, save := assignedPage, hiddenAmong, repairBot, unassign, unassignInTrash, adminsOf, activeUsers, workspaceAdmins, tell, loadProgress, saveProgress
	made, idle := madeSince, importsIdle
	t.Cleanup(func() {
		assignedPage, hiddenAmong, repairBot, unassign, unassignInTrash, adminsOf, activeUsers, workspaceAdmins, tell, loadProgress, saveProgress = page, hid, bot, un, unTrash, admins, active, everyone, told, load, save
		madeSince, importsIdle = made, idle
	})
	// Pages of the tasks that have an assignee, in the tasks' order, from the
	// place in the tasks the cursor names: a place, as the graph's uid cursor
	// is, so a task taken off moves no other one out of the next page.
	assignedPage = func(_ context.Context, after string, _ int) ([]taskDomain.AssignedTask, string, error) {
		w.events = append(w.events, "page")
		from := 0
		if after != "" {
			from, _ = strconv.Atoi(after)
		}
		var page []taskDomain.AssignedTask
		for i := from; i < len(w.tasks); i++ {
			if w.tasks[i].Task.Assignee == nil {
				continue
			}
			if w.page > 0 && len(page) == w.page {
				return page, strconv.Itoa(i), nil
			}
			page = append(page, w.tasks[i])
		}
		return page, "", nil
	}
	// A copy of each person's tasks, as a read answers: a later change in the
	// world never reaches an answer already given.
	hiddenAmong = func(_ context.Context, people []string) (map[string]map[string]bool, error) {
		w.reads = append(w.reads, people)
		out := map[string]map[string]bool{}
		for _, who := range people {
			out[who] = map[string]bool{}
			for task, yes := range w.hidden[who] {
				out[who][task] = yes
			}
		}
		return out, nil
	}
	madeSince = func(_ context.Context, tasks []string, since time.Time) (map[string]bool, error) {
		w.events, w.since = append(w.events, "made"), append(w.since, since)
		out := map[string]bool{}
		for _, id := range tasks {
			out[id] = w.made[id]
		}
		return out, nil
	}
	importsIdle = func(context.Context) (bool, error) {
		w.events = append(w.events, "idle")
		return false, w.idleErr
	}
	repairBot = func(context.Context) (*userBusiness.BotIdentity, error) {
		if w.noBot {
			return nil, errors.New("no principal")
		}
		w.bots++
		return &userBusiness.BotIdentity{DgraphUID: "0xbb", UUID: "bb000000-0000-4000-8000-0000000000bb", Name: "OneCamp, upgrade repair"}, nil
	}
	off := func(task *dgraphStruct.DgraphTask) error {
		if w.failing[task.Uuid] {
			return errors.New("the graph refused it")
		}
		for i := range w.tasks {
			if w.tasks[i].Task.Uuid == task.Uuid {
				w.tasks[i].Task.Assignee = nil
			}
		}
		return nil
	}
	// Each take-off is the workspace's alone: a linked GitHub issue keeps its
	// own assignees (helpers.WithWorkspaceOnly).
	unassign = func(ctx context.Context, _ uuid.UUID, to *dgraphStruct.DgraphUser, _, _ string, task *dgraphStruct.DgraphTask, by *dgraphStruct.DgraphUser) error {
		if to != nil || by.Uid != "0xbb" || !helpers.IsWorkspaceOnly(ctx) {
			t.Errorf("handed %s to %+v by %+v (workspace only: %v)", task.Uuid, to, by, helpers.IsWorkspaceOnly(ctx))
		}
		return off(task)
	}
	unassignInTrash = func(ctx context.Context, task *dgraphStruct.DgraphTask, to, by *dgraphStruct.DgraphUser) error {
		if to != nil || by.Uid != "0xbb" || !helpers.IsWorkspaceOnly(ctx) {
			t.Errorf("handed %s in the trash to %+v by %+v (workspace only: %v)", task.Uuid, to, by, helpers.IsWorkspaceOnly(ctx))
		}
		return off(task)
	}
	adminsOf = func(context.Context, []string) (map[string]projectDomain.ProjectAdmins, error) {
		if w.noAdmins {
			return nil, errors.New("down")
		}
		return map[string]projectDomain.ProjectAdmins{launch: {Name: "Launch", Admins: []string{admin}}, attic: {Name: "Attic"}}, nil
	}
	activeUsers = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]*userModel.User, error) {
		out := map[uuid.UUID]*userModel.User{}
		for _, id := range ids {
			out[id] = &userModel.User{Id: id, EmailID: "admin@example.com"}
		}
		return out, nil
	}
	workspaceAdmins = func(context.Context) ([]*userModel.User, error) {
		return []*userModel.User{{Id: uuid.MustParse(owner), EmailID: "owner@example.com"}}, nil
	}
	// The launch's notices go to its admin; the attic's, which has none who
	// can be told, to the workspace's admin. In the Inbox alone, either way.
	tell = func(_ context.Context, n notificationBusiness.Notice, to []notificationBusiness.NoticeRecipient) []string {
		w.told = append(w.told, n)
		want := admin
		if n.ResourceUUID == attic {
			want = owner
		}
		if len(to) != 1 || to[0].UserUUID != want || !to[0].InboxOnly {
			t.Errorf("%s told %+v, want %s in the Inbox alone", n.ResourceUUID, to, want)
		}
		return []string{want}
	}
	loadProgress = func(string) (progress, error) {
		var p progress
		if w.record != nil {
			if err := json.Unmarshal(w.record, &p); err != nil {
				t.Fatal(err)
			}
		}
		return p, nil
	}
	saveProgress = func(p progress) error {
		b, err := json.Marshal(p)
		w.record = b
		return err
	}
}

func assigned(id, to string, inProject bool, flags ...string) taskDomain.AssignedTask {
	who := &dgraphStruct.DgraphUser{Uid: "0x" + to[:2], Uuid: to, UserName: "Ben Ade"}
	task := &dgraphStruct.DgraphTask{Uid: "0x" + id[:2], Uuid: id, Assignee: who, Project: &dgraphStruct.DgraphProject{Uid: "0x1", Uuid: launch}}
	for _, f := range flags {
		switch f {
		case "bot":
			who.IsBot = true
		case "external":
			who.IsExternal = true
		}
	}
	return taskDomain.AssignedTask{Task: task, InProject: inProject}
}

// movedTo puts a's task in another project.
func movedTo(project string, a taskDomain.AssignedTask) taskDomain.AssignedTask {
	a.Task.Project = &dgraphStruct.DgraphProject{Uid: "0x2", Uuid: project}
	return a
}

const (
	ben, kim, maya, agent, author = "be000000-0000-4000-8000-000000000001", "c1000000-0000-4000-8000-000000000002", "d1000000-0000-4000-8000-000000000003", "e1000000-0000-4000-8000-000000000004", "f1000000-0000-4000-8000-000000000005"
	kit, vault, plan, robot, card = "10000000-0000-4000-8000-00000000000a", "20000000-0000-4000-8000-00000000000b", "30000000-0000-4000-8000-00000000000c", "40000000-0000-4000-8000-00000000000d", "50000000-0000-4000-8000-00000000000e"
)

// Who is taken off is judged by the assign rule, as assigning them now would
// be: outside the project, or kept out of a restricted task. A member of the
// project, a bot and someone who can't sign in keep theirs, and each person's
// restricted tasks are read once for the page.
func TestTheRepairTakesOffOnlyWhomTheAssignRuleWouldRefuse(t *testing.T) {
	w := &world{
		tasks: []taskDomain.AssignedTask{assigned(kit, ben, false), assigned(vault, kim, true), assigned(plan, maya, true),
			assigned(robot, agent, false, "bot"), assigned(card, author, false, "external")},
		hidden: map[string]map[string]bool{kim: {vault: true}},
	}
	w.install(t)
	w.tasks = append(w.tasks, assigned("60000000-0000-4000-8000-00000000000f", maya, true))

	rep, err := repairAssignees(context.Background())
	if err != nil || rep.Read != 6 || rep.Removed != 2 || rep.Told != 2 || rep.Passed != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	for _, a := range w.tasks {
		gone := a.Task.Assignee == nil
		if want := a.Task.Uuid == kit || a.Task.Uuid == vault; gone != want {
			t.Errorf("%s taken off %v, want %v", a.Task.Uuid, gone, want)
		}
	}
	if len(w.reads) != 1 || len(w.reads[0]) != 2 || w.reads[0][0] != kim || w.reads[0][1] != maya {
		t.Errorf("the restricted tasks were read for %v, want once for the page's two people in it", w.reads)
	}
	if w.bots != 1 || len(w.told) != 2 || w.told[0].Data["tasks"] != "1" || w.told[0].ResourceUUID != launch || w.told[0].Actor.Uid != "0xbb" {
		t.Errorf("principal made %d times; told %+v", w.bots, w.told)
	}
}

// One task whose take-off keeps failing holds the repair back only until it
// has failed passAfter runs in a row: the rest are done and told on the
// first, and it is then passed and left as it is.
func TestATaskThatKeepsFailingIsPassed(t *testing.T) {
	w := &world{tasks: []taskDomain.AssignedTask{assigned(kit, ben, false), assigned(vault, kim, false)}, failing: map[string]bool{vault: true}}
	w.install(t)
	for run := 1; run < passAfter; run++ {
		if rep, err := repairAssignees(context.Background()); err == nil || rep.Removed != 1 {
			t.Fatalf("run %d: %+v %v, want an error to run again", run, rep, err)
		}
	}
	rep, err := repairAssignees(context.Background())
	if err != nil || rep.Removed != 1 || rep.Passed != 1 || rep.Told != 1 {
		t.Fatalf("run %d: %+v %v, want it passed", passAfter, rep, err)
	}
	w.failing = nil
	if rep, err := repairAssignees(context.Background()); err != nil || rep.Removed != 1 || w.tasks[1].Task.Assignee == nil {
		t.Errorf("a passed task was tried again: %+v %v", rep, err)
	}
}

// A principal that can't be made stops the run without counting against any
// task; and those taken off before a restart are still told after it.
func TestWhatARunTookOffIsToldAfterARestart(t *testing.T) {
	w := &world{tasks: []taskDomain.AssignedTask{assigned(kit, ben, false)}, noBot: true}
	w.install(t)
	if _, err := repairAssignees(context.Background()); err == nil || w.tasks[0].Task.Assignee == nil {
		t.Fatalf("ran with no principal: %v", err)
	}
	var p progress
	_ = json.Unmarshal(w.record, &p)
	if len(p.Failing) != 0 {
		t.Errorf("a missing principal counted against tasks: %v", p.Failing)
	}

	w.noBot, w.noAdmins = false, true
	if _, err := repairAssignees(context.Background()); err == nil || w.tasks[0].Task.Assignee != nil || len(w.told) != 0 {
		t.Fatalf("took off %v, told %v, err %v", w.tasks[0].Task.Assignee == nil, w.told, err)
	}
	w.noAdmins = false
	rep, err := repairAssignees(context.Background())
	if err != nil || rep.Removed != 1 || rep.Told != 1 || len(w.told) != 1 || w.told[0].Data["person"] != ben || w.told[0].Data["tasks"] != "1" {
		t.Errorf("after the restart: %+v %v, told %+v", rep, err, w.told)
	}
}

// The people's restricted tasks are read once per page, for the page's
// people as they stand then (someone on an earlier page too): never once per
// person, and not at all for a page with nobody to judge by them (outside
// the project, a bot).
func TestTheRestrictedTasksAreReadOncePerPageForItsPeople(t *testing.T) {
	w := &world{
		tasks: []taskDomain.AssignedTask{assigned(kit, ben, true), assigned(vault, kim, true),
			assigned(plan, ben, true), assigned(card, maya, true), assigned(robot, agent, true, "bot")},
		hidden: map[string]map[string]bool{kim: {vault: true}},
		page:   2,
	}
	w.install(t)
	rep, err := repairAssignees(context.Background())
	if err != nil || rep.Read != 5 || rep.Removed != 1 || w.tasks[1].Task.Assignee != nil {
		t.Fatalf("%+v %v", rep, err)
	}
	if len(w.reads) != 2 || len(w.reads[0]) != 2 || w.reads[0][0] != ben || w.reads[0][1] != kim ||
		len(w.reads[1]) != 2 || w.reads[1][0] != ben || w.reads[1][1] != maya {
		t.Errorf("restricted tasks read for %v, want [[ben kim] [ben maya]]", w.reads)
	}
}

// A page's people are judged by their restricted tasks as they stand when
// the page is judged: someone let into a task after an earlier page judged
// them keeps it, whether they were let in before its page was read or while
// the repair waited for an import after reading it.
func TestSomeoneLetInAfterAnEarlierPageKeepsTheirTask(t *testing.T) {
	for _, during := range []string{"before the page", "while an import ran"} {
		t.Run(during, func(t *testing.T) {
			w := &world{
				tasks:  []taskDomain.AssignedTask{assigned(kit, kim, true), assigned(vault, kim, true)},
				hidden: map[string]map[string]bool{kim: {vault: true}},
				page:   1,
			}
			w.install(t)
			// An admin lets Kim into the vault once page 1 (the kit) has
			// judged her.
			letIn := func() { w.hidden = map[string]map[string]bool{} }
			pages, read := 0, assignedPage
			assignedPage = func(ctx context.Context, after string, n int) ([]taskDomain.AssignedTask, string, error) {
				if pages++; pages == 2 && during == "before the page" {
					letIn()
				}
				return read(ctx, after, n)
			}
			waited := false
			importsIdle = func(context.Context) (bool, error) {
				w.events = append(w.events, "idle")
				if pages == 2 && during == "while an import ran" && !waited {
					waited = true
					letIn()
					return true, nil
				}
				return false, nil
			}
			rep, err := repairAssignees(context.Background())
			if err != nil || rep.Removed != 0 || w.tasks[1].Task.Assignee == nil {
				t.Fatalf("%+v %v: Kim, let into the vault after page 1, was taken off it", rep, err)
			}
			if len(w.reads) < 2 {
				t.Errorf("Kim's restricted tasks were read for %v, want once for each page", w.reads)
			}
		})
	}
}

// The project's admins are told who was taken off how many of its tasks, in
// it by name, and how to give the tasks back. A project with no admin who
// can be told tells the workspace's admins, who may not be able to open it:
// theirs is closed, naming no project, opening nothing and asking nothing.
func TestTheAdminsAreToldByNameAndTheWorkspacesAdminsClosed(t *testing.T) {
	w := &world{tasks: []taskDomain.AssignedTask{assigned(kit, ben, false), movedTo(attic, assigned(vault, ben, false)), assigned(plan, ben, false)}}
	w.install(t)
	rep, err := repairAssignees(context.Background())
	if err != nil || rep.Removed != 3 || rep.Told != 2 || len(w.told) != 2 {
		t.Fatalf("%+v %v; told %+v", rep, err, w.told)
	}
	by := map[string]notificationBusiness.Notice{}
	for _, n := range w.told {
		by[n.ResourceUUID] = n
	}
	own, closed := by[launch], by[attic]
	if own.Closed || own.ResourceTitle != "Launch" || own.Title != "Ben Ade was taken off 2 tasks in Launch" ||
		own.Body != "To give them the tasks back, add them to the project or let them into the tasks, then assign them again." ||
		own.Data["tasks"] != "2" || own.Data["person"] != ben {
		t.Errorf("the launch's admin was told %+v", own)
	}
	if !closed.Closed || closed.ResourceTitle != "" || closed.Title != "Ben Ade was taken off 1 task in a project you can't open" ||
		closed.Body != "" || closed.Data["tasks"] != "1" || closed.Data["person"] != ben {
		t.Errorf("the workspace's admin was told %+v", closed)
	}
}

// What an import made after the repair's first start is a data move's, and
// keeps its people: the repair covers only what was there at the upgrade.
// That start is kept, so a later process judges by the first one's.
func TestWhatAnImportMadeSinceTheFirstStartIsKept(t *testing.T) {
	first := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	was := started
	t.Cleanup(func() { started = was })
	started = first
	w := &world{tasks: []taskDomain.AssignedTask{assigned(kit, ben, false), assigned(vault, kim, false)}, made: map[string]bool{kit: true}}
	w.install(t)
	w.idleErr = errors.New("stopping")
	if _, err := repairAssignees(context.Background()); err == nil {
		t.Fatal("ran on while the wait for imports failed")
	}
	w.idleErr = nil
	started = first.Add(time.Hour) // a later process
	rep, err := repairAssignees(context.Background())
	if err != nil || rep.Removed != 1 || w.tasks[0].Task.Assignee == nil || w.tasks[1].Task.Assignee != nil {
		t.Fatalf("%+v %v: an import's task taken off %v", rep, err, w.tasks[0].Task.Assignee == nil)
	}
	if len(w.since) == 0 || !w.since[len(w.since)-1].Equal(first) {
		t.Errorf("judged what imports made since %v, want the first start %v", w.since, first)
	}
}

// Each page is judged only once no import is running: after its read, so
// what a running import made in it has been recorded as the import's by
// then, and before reading what imports made. A wait that fails takes off
// nothing.
func TestTheRepairWaitsForImportsAfterEachPageRead(t *testing.T) {
	w := &world{tasks: []taskDomain.AssignedTask{assigned(kit, ben, false), assigned(vault, kim, false)}, page: 1, idleErr: context.Canceled}
	w.install(t)
	if _, err := repairAssignees(context.Background()); !errors.Is(err, context.Canceled) || w.tasks[0].Task.Assignee == nil {
		t.Fatalf("%v; taken off while waiting: %v", err, w.tasks[0].Task.Assignee == nil)
	}
	w.idleErr, w.events = nil, nil
	if _, err := repairAssignees(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(w.events, " "); got != "page idle made page idle made" {
		t.Errorf("order %q, want each page read, then the wait, then what imports made", got)
	}
}

// A page read while an import ran is read again once none runs, and judged
// as it stands then: what people changed while the repair waited holds
// (someone let into the project, a task handed to someone in it).
func TestAPageReadWhileAnImportRanIsReadAgainAfterIt(t *testing.T) {
	w := &world{tasks: []taskDomain.AssignedTask{assigned(kit, ben, false), assigned(vault, kim, false)}}
	w.install(t)
	running := true
	importsIdle = func(context.Context) (bool, error) {
		w.events = append(w.events, "idle")
		if !running {
			return false, nil
		}
		// While the import ran, Ben was let into the project, and the vault
		// was handed to Maya, who is in it.
		running = false
		w.tasks[0].InProject = true
		w.tasks[1] = assigned(vault, maya, true)
		return true, nil
	}
	rep, err := repairAssignees(context.Background())
	if err != nil || rep.Removed != 0 || w.tasks[0].Task.Assignee == nil || w.tasks[1].Task.Assignee == nil || w.tasks[1].Task.Assignee.Uuid != maya {
		t.Fatalf("%+v %v: what changed while an import ran was undone", rep, err)
	}
	if got := strings.Join(w.events, " "); got != "page idle page idle made" {
		t.Errorf("order %q, want the page read again after the wait, then what imports made", got)
	}
}
