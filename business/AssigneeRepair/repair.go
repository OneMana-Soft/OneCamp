package business

// The upgrade's pass over old assignments. A task goes only to someone who
// may read it already (business/Task assigneeSees.go), and assignments made
// before that rule are brought into line with it once per install, as the
// server starts: each assignee the rule would refuse now (someone outside
// the task's project, or one of its people its restriction keeps out) is
// taken off: the OneCamp assignment alone, through the one unassign every
// path takes, so the task's history names "OneCamp, upgrade repair" (never a
// person) and the person's calendar copy goes as every unassign's does,
// while a linked GitHub issue keeps its assignees, which are the issue's own
// (helpers.WithWorkspaceOnly). Then each project's admins are told in their
// Inbox who was taken off how many of its tasks, with no task named.
//
// The rule is judged as assigning judges it: one of the task's project's
// members or admins (domain/Task TaskReader.MayRead), and, for a restricted
// task, one its rows or the rule let in (business/WorkAccess
// HiddenUnassignedAmong). Bots keep their tasks (an AI teammate works what it's
// given), so do people who can't sign in (an imported author is a name), and
// so does every task in no project, which is its maker's own.
//
// Only what was there at the upgrade is judged. A data move brings its people
// with it (business/Task assigneeSees.go), so a task an import made after the
// repair's first start keeps its assignee: the import's own record says when
// it made one (the task keeps its source's dates), and each page is judged
// only once no import is running, by when a running import has recorded each
// task it made as its own (business/ImportRun Idle).

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	importRun "github.com/akashc777/OneCamp/business/ImportRun"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	repairBusiness "github.com/akashc777/OneCamp/business/Repair"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// NoticeEvent is the admins' notice: someone was taken off a project's tasks.
// The inbox names it, and opens it, as it does every project's notice
// (business/Project inboxNotices.go).
const NoticeEvent = "project.assignees_repaired"

// giveBack is the line under the notice for an admin who can open the
// project: how to give the tasks back, in the words the assign path's own
// refusal uses (business/Task assigneeSees.go).
const giveBack = "To give them the tasks back, add them to the project or let them into the tasks, then assign them again."

const (
	repairKey = "tasks.assignees_read_v1"
	// passAfter is on how many runs a task's take-off may fail before the
	// repair passes it, so one task that always fails never keeps the rest
	// from being done (the server log names it).
	passAfter = 3
)

// repairPage is how many tasks one read takes: a seam for tests.
var repairPage = 200

// Repair takes off, once per install, every assignee the assign rule would
// refuse now.
var Repair = repairBusiness.Once[Report]{Key: repairKey, Name: "task.assignee-repair", Tries: 12, Repair: repairAssignees}

// Report is what the repair has done.
type Report struct {
	Read    int `json:"read"`    // assignments in projects read by the last run
	Removed int `json:"removed"` // assignees taken off
	Told    int `json:"told"`    // project and person pairs the admins were told of
	Passed  int `json:"passed"`  // tasks passed after their take-off failed passAfter runs
}

func (r Report) String() string {
	return fmt.Sprintf("%d assignments in projects read, %d assignees taken off by the assign rule, %d told to their projects' admins, %d passed",
		r.Read, r.Removed, r.Told, r.Passed)
}

// untold is one project's assignee taken off its tasks whom its admins
// haven't been told of yet.
type untold struct {
	Project string `json:"project"`
	Person  string `json:"person"`
	Name    string `json:"name"`
	Tasks   int    `json:"tasks"`
}

// progress is what earlier runs did that a later one goes on from: since
// when imports' tasks are theirs (the first start), those taken off whose
// admins aren't told yet (a restart between the two still tells), the tasks
// whose take-off failed and on how many runs, the tasks passed, and the
// report.
type progress struct {
	Since   time.Time          `json:"since"`
	Untold  map[string]*untold `json:"untold"`
	Failing map[string]int     `json:"failing"`
	Passed  map[string]bool    `json:"passed"`
	Report  Report             `json:"report"`
}

// Seams for tests: the reads, the writes and the telling.
var (
	assignedPage    = taskDomain.AssignedInProjectsPage
	hiddenAmong     = workAccess.HiddenUnassignedAmong
	madeSince       = importModels.MadeSince
	importsIdle     = importRun.Idle
	repairBot       = userBusiness.EnsureUpgradeRepairBot
	unassign        = taskBusiness.UpdateTaskAssigneeByTaskUUID
	unassignInTrash = taskBusiness.AssignInTrash
	adminsOf        = projectDomain.AdminsOf
	activeUsers     = userDomain.GetActiveUsersByUUIDsForNotification
	workspaceAdmins = func(ctx context.Context) ([]*userModel.User, error) {
		users, _, err := userDomain.GetAllAdminUsers(ctx, 0, 200)
		return users, err
	}
	tell         = notificationBusiness.Tell
	loadProgress = repairBusiness.Progress[progress]
	saveProgress = func(p progress) error { return repairBusiness.SaveProgress(repairKey, p) }
)

// started is when this process started, before it served anyone: what an
// import makes from then on is its own. A seam for tests.
var started = time.Now()

// run is one run of the repair: its progress, and the system principal once
// a take-off needs it.
type run struct {
	p  progress
	by *dgraphStruct.DgraphUser
}

// repairAssignees reads every task in a project that has an assignee, a
// page at a time, takes off each assignee the assign rule would refuse now,
// then tells the admins. A read that fails stops the run, to run again; a
// take-off that fails is counted and passed for this run, so the rest are
// done, and the run then answers an error to run again.
func repairAssignees(ctx context.Context) (Report, error) {
	ctx = helpers.WithWorkspaceOnly(ctx)
	p, err := loadProgress(repairKey)
	if err != nil {
		return Report{}, err
	}
	if p.Untold == nil {
		p.Untold = map[string]*untold{}
	}
	if p.Failing == nil {
		p.Failing = map[string]int{}
	}
	if p.Passed == nil {
		p.Passed = map[string]bool{}
	}
	// The first run keeps its process's start, so a later process (a restart
	// before the repair is done) judges imports by the first one's.
	if p.Since.IsZero() {
		p.Since = started
		if err := saveProgress(p); err != nil {
			return p.Report, err
		}
	}
	r := &run{p: p}
	r.p.Report.Read = 0
	failed := 0
	for after := ""; ; {
		page, next, err := assignedPage(ctx, after, repairPage)
		if err != nil {
			return r.p.Report, err
		}
		// After the read, it waits while an import runs, so what one made in
		// the page is recorded as the import's by then. A page read while one
		// ran is read again: it is judged as it stands, never as it stood
		// before the wait (people may have changed it meanwhile).
		waited, err := importsIdle(ctx)
		if err != nil {
			return r.p.Report, err
		}
		if waited {
			continue
		}
		n, err := r.page(ctx, page)
		failed += n
		if err != nil {
			return r.p.Report, err
		}
		if next == "" {
			break
		}
		after = next
	}
	if err := r.tellAdmins(ctx); err != nil {
		return r.p.Report, err
	}
	if failed > 0 {
		return r.p.Report, fmt.Errorf("%d tasks' assignees weren't taken off (the server log names them)", failed)
	}
	return r.p.Report, nil
}

// page judges one page of assignments that no import was still making once
// it was read, takes off each assignee the assign rule would refuse now, and
// keeps the progress. Its reads are one each for the page: which of its
// tasks imports made since the first start, and its people's restricted
// tasks, read again for every page, so someone let in since an earlier page
// (or while the repair waited for an import) is judged as they stand. It
// answers how many take-offs failed and hold the repair back.
func (r *run) page(ctx context.Context, page []taskDomain.AssignedTask) (int, error) {
	ids := make([]string, 0, len(page))
	for _, a := range page {
		ids = append(ids, a.Task.Uuid)
	}
	made, err := madeSince(ctx, ids, r.p.Since)
	if err != nil {
		return 0, err
	}
	judge := make([]taskDomain.AssignedTask, 0, len(page))
	for _, a := range page {
		r.p.Report.Read++
		if !r.p.Passed[a.Task.Uuid] && !made[strings.ToLower(a.Task.Uuid)] {
			judge = append(judge, a)
		}
	}
	hidden, err := readHidden(ctx, judge)
	if err != nil {
		return 0, err
	}
	var off []*dgraphStruct.DgraphTask
	for _, a := range judge {
		if refused(a, hidden) {
			off = append(off, a.Task)
		}
	}
	failed := 0
	if len(off) > 0 {
		// The principal is made only when there is something to take off;
		// failing to make it stops the run without counting against a task.
		if err := r.principal(ctx); err != nil {
			return 0, err
		}
		for _, task := range off {
			failed += r.takeOff(ctx, task)
		}
	}
	return failed, saveProgress(r.p)
}

// readHidden reads, in one read for the page, the restricted tasks each of
// its people judged by them (one of a project's people on a task in it) is
// kept out of, as assigning judges it: by lower-case person, then task.
func readHidden(ctx context.Context, judge []taskDomain.AssignedTask) (map[string]map[string]bool, error) {
	var people []string
	seen := map[string]bool{}
	for _, a := range judge {
		who := a.Task.Assignee
		if !a.InProject || who.IsBot || who.IsExternal || seen[strings.ToLower(who.Uuid)] {
			continue
		}
		seen[strings.ToLower(who.Uuid)], people = true, append(people, who.Uuid)
	}
	if len(people) == 0 {
		return nil, nil
	}
	return hiddenAmong(ctx, people)
}

// refused says whether the assign rule would refuse a's assignee now:
// outside its project, or kept out of the restricted task (hidden, as the
// page read it). Bots and people who can't sign in keep theirs.
func refused(a taskDomain.AssignedTask, hidden map[string]map[string]bool) bool {
	who := a.Task.Assignee
	if who.IsBot || who.IsExternal {
		return false
	}
	if !a.InProject {
		return true
	}
	return hidden[strings.ToLower(who.Uuid)][strings.ToLower(a.Task.Uuid)]
}

// principal resolves the system principal the repair acts as, "OneCamp,
// upgrade repair", provisioning it the first time: an install with nothing
// to repair never gets it.
func (r *run) principal(ctx context.Context) error {
	if r.by != nil {
		return nil
	}
	bot, err := repairBot(ctx)
	if err != nil {
		return err
	}
	r.by = &dgraphStruct.DgraphUser{Uid: bot.DgraphUID, Uuid: bot.UUID, UserName: bot.Name,
		EmailID: userDomain.UpgradeRepairBotEmail, IsBot: true}
	return nil
}

// takeOff takes the task's assignee off as the system principal and counts
// it: owed a telling, or failed (fails). It answers 1 for a failure that
// holds the repair back.
func (r *run) takeOff(ctx context.Context, task *dgraphStruct.DgraphTask) int {
	who, project := task.Assignee, task.Project.Uuid
	if err := r.unassign(ctx, task); err != nil {
		if r.fails(ctx, task.Uuid, err) {
			return 1
		}
		return 0
	}
	delete(r.p.Failing, task.Uuid)
	r.p.Report.Removed++
	key := project + " " + who.Uuid
	if r.p.Untold[key] == nil {
		r.p.Untold[key] = &untold{Project: project, Person: who.Uuid, Name: who.DisplayName()}
	}
	r.p.Untold[key].Tasks++
	return 0
}

// unassign takes the assignee off through the one unassign every path
// takes: a task in the trash stays there, so a restore brings it back
// without them.
func (r *run) unassign(ctx context.Context, task *dgraphStruct.DgraphTask) error {
	if helpers.IsSoftDeleted(task.DeletedAt) {
		return unassignInTrash(ctx, task, nil, r.by)
	}
	id, err := uuid.Parse(task.Uuid)
	if err != nil {
		return err
	}
	return unassign(ctx, id, nil, task.Assignee.Uid, task.Uid, task, r.by)
}

// fails counts a take-off that failed, and says whether it holds the repair
// back: it does until it has failed on passAfter runs, and is then passed,
// named in the server log.
func (r *run) fails(ctx context.Context, task string, err error) bool {
	r.p.Failing[task]++
	if r.p.Failing[task] < passAfter {
		helpers.LogWarnWithContext(ctx, "task.assignee-repair: task %s's assignee wasn't taken off (run %d of %d): %v", task, r.p.Failing[task], passAfter, err)
		return true
	}
	helpers.LogErrorWithContext(ctx, "task.assignee-repair: passed task %s after %d runs failed to take its assignee off: %+v", task, passAfter, err)
	delete(r.p.Failing, task)
	r.p.Passed[task] = true
	r.p.Report.Passed++
	return false
}

// tellAdmins tells each project's admins, once per person taken off its
// tasks, in their Inbox: who, and how many tasks, never which. A project
// with no admin who can be told tells the workspace's admins instead, who
// may not be able to open it, so their notice is closed (fail closed). One
// telling per project and person, to its group. The progress is kept once
// all are told: telling again after a restart before that tells nobody
// twice (a notice's dedup key holds it).
func (r *run) tellAdmins(ctx context.Context) error {
	if len(r.p.Untold) == 0 {
		return nil
	}
	if err := r.principal(ctx); err != nil {
		return err
	}
	keys := make([]string, 0, len(r.p.Untold))
	var projects []string
	seen := map[string]bool{}
	for key, u := range r.p.Untold {
		keys = append(keys, key)
		if p := strings.ToLower(u.Project); !seen[p] {
			seen[p], projects = true, append(projects, u.Project)
		}
	}
	sort.Strings(keys)
	byProject, err := adminsOf(ctx, projects)
	if err != nil {
		return err
	}
	to, err := adminsToTell(ctx, projects, byProject)
	if err != nil {
		return err
	}
	for _, key := range keys {
		u := r.p.Untold[key]
		project := strings.ToLower(u.Project)
		group := to[project]
		tell(ctx, r.notice(u, byProject[project].Name, group.closed), group.to)
		delete(r.p.Untold, key)
		r.p.Report.Told++
	}
	return saveProgress(r.p)
}

// notice is the admins' notice that u was taken off a project's tasks:
// counted, never named. Its own admins read the project's name and how to
// give the tasks back. A closed one, for whoever may not be able to open the
// project, names no project, opens nothing and asks nothing, as the inbox's
// own reading of it does for a reader who can't open it.
func (r *run) notice(u *untold, name string, closed bool) notificationBusiness.Notice {
	n := notificationBusiness.Notice{
		Kind:         NoticeEvent,
		ResourceKind: workAccess.KindProject,
		ResourceUUID: u.Project,
		Actor:        r.by,
		Data:         map[string]string{"person": u.Person, "person_name": u.Name, "tasks": strconv.Itoa(u.Tasks)},
		DedupKey:     "assignee-repair|" + u.Project + "|" + u.Person,
		Path:         "/app/project/" + u.Project,
	}
	taken := u.Name + " was taken off " + helpers.Count(u.Tasks, "task", "tasks") + " in "
	if closed {
		n.Closed, n.Title = true, taken+"a project you can't open"
		return n
	}
	title := name
	if title == "" {
		title = "Untitled"
	}
	n.ResourceTitle, n.Title, n.Body = name, taken+title, giveBack
	return n
}

// audience is whom one project's notice goes to, and whether it goes
// closed: to the workspace's admins, who may not be able to open the project.
type audience struct {
	to     []notificationBusiness.NoticeRecipient
	closed bool
}

// adminsToTell is, by lower-case project, whom each project's notice goes
// to: its admins who can be told (active, and never the demo's own people),
// or, for a project with none, the workspace's admins, closed. Two reads at
// most, however many projects: their admins together, and the workspace's
// once.
func adminsToTell(ctx context.Context, projects []string, byProject map[string]projectDomain.ProjectAdmins) (map[string]audience, error) {
	var ids []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, project := range projects {
		for _, a := range byProject[strings.ToLower(project)].Admins {
			if id, err := uuid.Parse(a); err == nil && !seen[id] {
				seen[id], ids = true, append(ids, id)
			}
		}
	}
	active := map[uuid.UUID]*userModel.User{}
	if len(ids) > 0 {
		var err error
		if active, err = activeUsers(ctx, ids); err != nil {
			return nil, err
		}
	}
	out := make(map[string]audience, len(projects))
	var none []string
	for _, project := range projects {
		key := strings.ToLower(project)
		var users []*userModel.User
		for _, a := range byProject[key].Admins {
			if id, err := uuid.Parse(a); err == nil && active[id] != nil {
				users = append(users, active[id])
			}
		}
		if recipients := notificationBusiness.InboxAdmins(users); len(recipients) > 0 {
			out[key] = audience{to: recipients}
		} else {
			none = append(none, key)
		}
	}
	if len(none) > 0 {
		everyone, err := workspaceAdmins(ctx)
		if err != nil {
			return nil, err
		}
		for _, key := range none {
			out[key] = audience{to: notificationBusiness.InboxAdmins(everyone), closed: true}
		}
	}
	return out, nil
}
