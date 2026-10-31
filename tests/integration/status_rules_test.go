//go:build integration
// +build integration

package integration_test

// A project's status rules and approvals end to end (package 9 R2), against
// real Postgres 12 and Dgraph: a rule refuses a move in words wherever it
// comes from; a move that needs approval becomes one request, and enough
// approvals make the move as the person who asked.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAStatusRule -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	approvalsBusiness "github.com/akashc777/OneCamp/business/Approvals"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
)

func TestAStatusRuleHoldsAMoveAndAnApprovalMakesIt(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana, ben := w.account("ana"), w.account("ben")
	project := uuid.MustParse(w.project)
	done := []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: json.RawMessage(`"done"`)}}

	// Done needs an assignee: ana's task has none, and the move is refused.
	if _, err := approvalsBusiness.SaveRule(ctx, ana.UserPostgresInfo.Id, project, approvalsBusiness.RuleInput{Status: "done", Who: "anyone", Required: []string{"assignee"}}); err != nil {
		t.Fatalf("an admin sets a rule: %v", err)
	}
	_, _, err := taskWriter.Tasks.Apply(ctx, &ben, uuid.MustParse(w.task), done, 0)
	if r, ok := taskBusiness.RefusedStatus(err); !ok || r.Why != "Give it an assignee before moving it to Done." {
		t.Fatalf("a move without an assignee: %v", err)
	}
	got, _ := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, ana.UserDgraphInfo.Uid)
	if got.Status == "done" {
		t.Fatal("a refused move moved the task")
	}

	// Someone outside the project can't be an approver.
	if _, err := approvalsBusiness.SaveRule(ctx, ana.UserPostgresInfo.Id, project, approvalsBusiness.RuleInput{Status: "done", Approvers: []string{w.people["eve"].String()}}); err == nil {
		t.Error("an approver outside the project was saved")
	}

	// Now Done needs ana's approval: ben's move on the task assigned to him
	// becomes one request.
	if _, err := approvalsBusiness.SaveRule(ctx, ana.UserPostgresInfo.Id, project, approvalsBusiness.RuleInput{Status: "done", Who: "anyone", Approvers: []string{ana.UserPostgresInfo.Id.String()}, ApprovalsNeeded: 1}); err != nil {
		t.Fatal(err)
	}
	mine := uuid.MustParse(w.mine)
	_, _, err = taskWriter.Tasks.Apply(ctx, &ben, mine, done, 0)
	r, ok := taskBusiness.RefusedStatus(err)
	if !ok || r.ApprovalID == "" {
		t.Fatalf("a move that needs approval: %v", err)
	}
	if _, _, err := taskWriter.Tasks.Apply(ctx, &ben, mine, done, 0); err == nil {
		t.Fatal("a second move went past the open request")
	}
	pending, err := model.PendingFor(ctx, mine)
	if err != nil || pending.ID.String() != r.ApprovalID || pending.RequestedBy != ben.UserPostgresInfo.Id {
		t.Fatalf("the request: %+v %v", pending, err)
	}

	// ben isn't an approver; ana is, and her yes makes the move as ben.
	if _, err := approvalsBusiness.Decide(ctx, &ben, mine, pending.ID, model.VerdictApprove, ""); err == nil {
		t.Error("someone not asked answered")
	}
	v, err := approvalsBusiness.Decide(ctx, &ana, mine, pending.ID, model.VerdictApprove, "Ship it")
	if err != nil || v.State != model.Approved {
		t.Fatalf("ana approves: %+v %v", v, err)
	}
	got, _ = taskBusiness.GetDgraphBasicTaskInfo(ctx, w.mine, ana.UserDgraphInfo.Uid)
	if got.Status != "done" {
		t.Errorf("approved, the task is %s", got.Status)
	}
	view, err := approvalsBusiness.ForTask(ctx, &ben, mine)
	if err != nil || view == nil || view.State != model.Approved || view.CanDecide || len(view.Decisions) != 1 {
		t.Errorf("the task's approval for ben: %+v %v", view, err)
	}
}

// A rule holds a move however its request reads the task: signed in, as the
// app's are; with no one on it, as a job that names no person; and as the
// system, as a sweeper's or GitHub's sync are. Each read carries the task's
// project, so each move meets the rule, whether Done wants a field,
// approvers or only the client.
func TestAStatusRuleHoldsAMoveWhoeverReadsTheTask(t *testing.T) {
	w := seedWorkWorld(t)
	asAna, ana := w.signedIn("ana")
	asBen, ben := w.signedIn("ben")
	project := uuid.MustParse(w.project)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := guestBusiness.CreateResourceGrant(asAna, ana.UserPostgresInfo.Id, guestModel.ResourceProject, w.project, guestModel.CapabilityComment, 0, false); err != nil {
		t.Fatal(err)
	}
	reads := []struct {
		name string
		ctx  context.Context
	}{
		{"signed in", asBen},
		{"with no one on it", context.Background()},
		{"as the system", taskread.WithSystem(context.Background())},
	}
	done := []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: json.RawMessage(`"done"`)}}
	waiting := "It's waiting for approval already."
	for _, c := range []struct {
		wants       string
		rule        approvalsBusiness.RuleInput
		task        string
		first, then string
	}{
		{"a description", approvalsBusiness.RuleInput{Status: "done", Who: "anyone", Required: []string{"description"}}, w.task,
			"Give it a description before moving it to Done.", "Give it a description before moving it to Done."},
		{"ana's approval", approvalsBusiness.RuleInput{Status: "done", Who: "anyone", Approvers: []string{ana.UserPostgresInfo.Id.String()}, ApprovalsNeeded: 1}, w.mine,
			"Sent for approval: it moves to Done once it's approved.", waiting},
		{"only the client", approvalsBusiness.RuleInput{Status: "done", Who: "anyone", Client: true}, w.assigned,
			"Sent for the client's approval: it moves to Done once they approve.", waiting},
	} {
		if r, err := approvalsBusiness.SaveRule(asAna, ana.UserPostgresInfo.Id, project, c.rule); err != nil || r == nil {
			t.Fatalf("Done wants %s: %+v %v", c.wants, r, err)
		}
		for i, read := range reads {
			want := c.then
			if i == 0 {
				want = c.first
			}
			_, _, err := taskWriter.Tasks.Apply(read.ctx, &ben, uuid.MustParse(c.task), done, 0)
			if r, ok := taskBusiness.RefusedStatus(err); !ok || r.Why != want {
				t.Errorf("Done wants %s, a move %s: %v, want %q", c.wants, read.name, err, want)
			}
		}
		if got, err := taskBusiness.GetDgraphBasicTaskInfo(asAna, c.task, ana.UserDgraphInfo.Uid); err != nil || got.Status == "done" {
			t.Errorf("Done wants %s, and the task moved: %+v %v", c.wants, got, err)
		}
	}
}

// A request closes when its task goes on without it (main, 27 Oct): moved
// by another path, the request closes as moved on and leaves the approver's
// waiting list, and a late yes is answered as closed and moves nothing (it
// pulled the task back). An ask about the task itself is not about its status
// and stays open through a move. Deleting the task closes either kind.
func TestAnApprovalRequestClosesWhenItsTaskGoesOnWithoutIt(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana, ben := w.account("ana"), w.account("ben")
	project := uuid.MustParse(w.project)
	to := func(status string) []taskWriter.Op {
		return []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: json.RawMessage(`"` + status + `"`)}}
	}
	if _, err := approvalsBusiness.SaveRule(ctx, ana.UserPostgresInfo.Id, project, approvalsBusiness.RuleInput{Status: "inReview", Who: "anyone", Approvers: []string{ana.UserPostgresInfo.Id.String()}, ApprovalsNeeded: 1}); err != nil {
		t.Fatal(err)
	}
	mine := uuid.MustParse(w.mine)
	ask := func() uuid.UUID {
		t.Helper()
		_, _, err := taskWriter.Tasks.Apply(ctx, &ben, mine, to("inReview"), 0)
		r, ok := taskBusiness.RefusedStatus(err)
		if !ok || r.ApprovalID == "" {
			t.Fatalf("a move to In review becomes a request: %v", err)
		}
		return uuid.MustParse(r.ApprovalID)
	}
	state := func(id uuid.UUID) string {
		t.Helper()
		a, err := model.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return a.State
	}

	// ben asks, then moves the task on to In progress, which needs no one.
	first := ask()
	if _, _, err := taskWriter.Tasks.Apply(ctx, &ben, mine, to("inProgress"), 0); err != nil {
		t.Fatalf("a move to In progress: %v", err)
	}
	if got := state(first); got != model.MovedOn {
		t.Errorf("the task moved on, and its request is %s", got)
	}
	waiting, err := model.Waiting(ctx, ana.UserPostgresInfo.Id, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range waiting {
		if a.ID == first {
			t.Error("ana's waiting list still holds the closed request")
		}
	}
	// ana's late yes is answered as closed, and the task stays in In progress.
	if _, err := approvalsBusiness.Decide(ctx, &ana, mine, first, model.VerdictApprove, ""); !errors.Is(err, approvalsBusiness.ErrNotOpen) {
		t.Errorf("a yes to a closed request: %v", err)
	}
	if got, _ := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.mine, ana.UserDgraphInfo.Uid); got == nil || got.Status != "inProgress" {
		t.Errorf("a late yes moved the task: %+v", got)
	}

	// An ask about the task itself is not about its status: it stays open
	// while the task moves, and a yes to it still counts.
	asked, err := approvalsBusiness.AskForTask(ctx, &ben, mine, approvalsBusiness.AskInput{Approvers: []string{ana.UserPostgresInfo.Id.String()}, Ask: "The copy"})
	if err != nil {
		t.Fatalf("ben asks for the task itself: %v", err)
	}
	if _, _, err := taskWriter.Tasks.Apply(ctx, &ben, mine, to("todo"), 0); err != nil {
		t.Fatalf("a move back to To do: %v", err)
	}
	if got := state(asked.ID); got != model.Pending {
		t.Errorf("the task moved, and the ask about it is %s", got)
	}
	if v, err := approvalsBusiness.Decide(ctx, &ana, mine, asked.ID, model.VerdictApprove, ""); err != nil || v.State != model.Approved {
		t.Errorf("ana's yes to the ask: %+v %v", v, err)
	}

	// Asked again, then deleted: the request closes with it.
	second := ask()
	task, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.mine, ana.UserDgraphInfo.Uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := taskBusiness.ArchiveTaskByTaskUUID(ctx, mine, task, &ben.UserDgraphInfo); err != nil {
		t.Fatalf("ben deletes the task: %v", err)
	}
	if got := state(second); got != model.MovedOn {
		t.Errorf("the task was deleted, and its request is %s", got)
	}

	// The approved move itself closes nothing more: approved stays approved.
	other := uuid.MustParse(w.task)
	_, _, err = taskWriter.Tasks.Apply(ctx, &ben, other, to("inReview"), 0)
	r, ok := taskBusiness.RefusedStatus(err)
	if !ok || r.ApprovalID == "" {
		t.Fatalf("ana's task asks too: %v", err)
	}
	third := uuid.MustParse(r.ApprovalID)
	if v, err := approvalsBusiness.Decide(ctx, &ana, other, third, model.VerdictApprove, ""); err != nil || v.State != model.Approved {
		t.Fatalf("ana approves: %+v %v", v, err)
	}
	if got := state(third); got != model.Approved {
		t.Errorf("the approved move left its request %s", got)
	}

	// Deleting a task closes an ask about it too.
	asked, err = approvalsBusiness.AskForTask(ctx, &ben, other, approvalsBusiness.AskInput{Approvers: []string{ana.UserPostgresInfo.Id.String()}, Ask: "The layout"})
	if err != nil {
		t.Fatalf("ben asks for ana's task itself: %v", err)
	}
	if task, err = taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, ana.UserDgraphInfo.Uid); err != nil {
		t.Fatal(err)
	}
	if err := taskBusiness.ArchiveTaskByTaskUUID(ctx, other, task, &ben.UserDgraphInfo); err != nil {
		t.Fatalf("ben deletes ana's task: %v", err)
	}
	if got := state(asked.ID); got != model.MovedOn {
		t.Errorf("the task was deleted, and the ask about it is %s", got)
	}
}

// A new task's first status meets its project's rules as a move into it from
// To do would (main's approvals check, gap 1), on each path that makes one:
// the app's create (the board's New task), the Task Writer (forms, a rule's
// "create task") and the bulk create. A rule on who refuses in words and
// nothing is made; required fields are read from the new task; a status that
// wants approval makes the task in To do and asks at once, saying both. A data
// move (a template's apply, an import) keeps its statuses.
func TestANewTasksFirstStatusMeetsItsRules(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana, ben := w.account("ana"), w.account("ben")
	project := uuid.MustParse(w.project)
	// Done is for the project's admins (ana; ben is a member); In review needs
	// a due date and ana's yes.
	for _, rule := range []approvalsBusiness.RuleInput{
		{Status: "done", Who: "admins"},
		{Status: "inReview", Who: "anyone", Required: []string{"due"}, Approvers: []string{ana.UserPostgresInfo.Id.String()}, ApprovalsNeeded: 1},
	} {
		if _, err := approvalsBusiness.SaveRule(ctx, ana.UserPostgresInfo.Id, project, rule); err != nil {
			t.Fatal(err)
		}
	}
	const adminsOnly = "Only the project's admins move tasks to Done."
	due := "2026-11-02T09:00:00Z"
	count := func() (n int) {
		t.Helper()
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM tasks WHERE project_id = $1`, w.project).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	statusOf := func(id string) string {
		t.Helper()
		got, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, id, ana.UserDgraphInfo.Uid)
		if err != nil || got == nil {
			t.Fatalf("reading task %s: %v", id, err)
		}
		return got.Status
	}
	refused := func(err error, why string) bool {
		r, ok := taskBusiness.RefusedStatus(err)
		return ok && r.Why == why && r.ApprovalID == ""
	}
	// asked is the open request ana is asked to answer for the task, if any.
	asked := func(task string) *model.Approval {
		t.Helper()
		waiting, err := model.Waiting(ctx, ana.UserPostgresInfo.Id, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range waiting {
			if a.TaskID.String() == task && a.State == model.Pending && a.ToStatus == "inReview" {
				return a
			}
		}
		return nil
	}

	t.Run("the app's create", func(t *testing.T) {
		create := func(who userModels.UserInfo, name, status, due string) (int, map[string]any) {
			t.Helper()
			raw, _ := json.Marshal(adapter.CreateOrUpdateTaskInput{TaskName: name, ProjectUuid: w.project, Status: status, DueDate: due})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, who))
			taskController.CreateTask(rec, req)
			var out map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			return rec.Code, out
		}
		made := func(out map[string]any) map[string]any {
			data, _ := out["data"].(map[string]any)
			return data
		}
		// ben, a member, can't make a task in Done, and none is made.
		before := count()
		if code, out := create(ben, "Ship the launch", "done", ""); code != http.StatusConflict || out["code"] != "status_rule" || out["msg"] != adminsOnly {
			t.Errorf("ben makes a task in Done: %d %v", code, out)
		}
		if count() != before {
			t.Error("a refused first status made the task")
		}
		// ana, an admin, can.
		code, out := create(ana, "Ship the launch", "done", "")
		if id, _ := made(out)["task_uuid"].(string); code != http.StatusOK || id == "" || statusOf(id) != "done" {
			t.Errorf("ana makes a task in Done: %d %v", code, out)
		}
		// What In review requires is read from the new task: no due date, no task.
		before = count()
		if code, out := create(ben, "Review the post", "inReview", ""); code != http.StatusConflict || out["msg"] != "Give it a due date before moving it to In review." {
			t.Errorf("a task without a due date made for In review: %d %v", code, out)
		}
		if count() != before {
			t.Error("a refused first status made the task")
		}
		// With one, it's made in To do and its move is ana's request; the answer says both.
		code, out = create(ben, "Review the post", "inReview", due)
		data := made(out)
		id, _ := data["task_uuid"].(string)
		if code != http.StatusOK || out["code"] != "approval" || data["status"] != "todo" || data["approval_id"] == nil ||
			out["msg"] != "Made in To do. Sent for approval: it moves to In review once it's approved." {
			t.Fatalf("a task made for In review: %d %v", code, out)
		}
		if got := statusOf(id); got != "todo" {
			t.Errorf("the held task is in %s", got)
		}
		a := asked(id)
		if a == nil || a.ID.String() != data["approval_id"] {
			t.Fatalf("ana isn't asked about the new task: %+v", a)
		}
		// ana's yes moves it on.
		if _, err := approvalsBusiness.Decide(ctx, &ana, uuid.MustParse(id), a.ID, model.VerdictApprove, ""); err != nil {
			t.Fatal(err)
		}
		if got := statusOf(id); got != "inReview" {
			t.Errorf("approved, the task is in %s", got)
		}
	})

	t.Run("the Task Writer", func(t *testing.T) {
		before := count()
		if _, err := taskWriter.MakeTask(ctx, &ben, project, taskWriter.NewTask{Name: "Close the quarter", Status: "done"}); !refused(err, adminsOnly) {
			t.Errorf("ben's form makes a task in Done: %v", err)
		}
		if count() != before {
			t.Error("a refused first status made the task")
		}
		when, _ := time.Parse(time.RFC3339, due)
		id, err := taskWriter.MakeTask(ctx, &ben, project, taskWriter.NewTask{Name: "Check the slides", Status: "inReview", Due: &when})
		if err != nil {
			t.Fatalf("a task made for In review: %v", err)
		}
		if got := statusOf(id.String()); got != "todo" || asked(id.String()) == nil {
			t.Errorf("a task made for In review is in %s, asked: %v", got, asked(id.String()) != nil)
		}
	})

	t.Run("the bulk create", func(t *testing.T) {
		info, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, w.project, ben.UserDgraphInfo.Uid)
		if err != nil {
			t.Fatal(err)
		}
		task := func(name, status, due string) taskBusiness.NewTask {
			return taskBusiness.NewTask{Input: adapter.CreateOrUpdateTaskInput{TaskName: name, ProjectUuid: w.project, Status: status, DueDate: due}, Parent: -1}
		}
		// One refused task refuses the batch before any of it is written.
		before := count()
		if _, err := taskBusiness.CreateTasks(ctx, project, &ben, info, []taskBusiness.NewTask{task("Plan the party", "", ""), task("Hold the party", "done", "")}); !refused(err, adminsOnly) {
			t.Errorf("a batch with a task in Done: %v", err)
		}
		if count() != before {
			t.Error("a refused batch made tasks")
		}
		// A task for In review has the batch made one task at a time: the others
		// as asked, and it in To do with its own request.
		ids, err := taskBusiness.CreateTasks(ctx, project, &ben, info, []taskBusiness.NewTask{task("Set the agenda", "inProgress", ""), task("Book the room", "inReview", due)})
		if err != nil || len(ids) != 2 {
			t.Fatalf("a batch with a task for In review: %v %v", ids, err)
		}
		if got := statusOf(ids[0].String()); got != "inProgress" || asked(ids[0].String()) != nil {
			t.Errorf("a batch's task for In progress is in %s, asked: %v", got, asked(ids[0].String()) != nil)
		}
		if got := statusOf(ids[1].String()); got != "todo" || asked(ids[1].String()) == nil {
			t.Errorf("a batch's task for In review is in %s, asked: %v", got, asked(ids[1].String()) != nil)
		}
		// A data move keeps its statuses: what was done is made done.
		ids, err = taskBusiness.CreateTasks(taskBusiness.DataMove(ctx), project, &ben, info, []taskBusiness.NewTask{task("Last year's launch", "done", "")})
		if err != nil || len(ids) != 1 || statusOf(ids[0].String()) != "done" {
			t.Errorf("a data move's task in Done: %v %v", ids, err)
		}
	})
}

// readProject is a read of w's project through handler h, as who: its status
// code and body.
func readProject(t *testing.T, w *workWorld, who string, h http.HandlerFunc, target string) (int, map[string]any) {
	t.Helper()
	rc := chi.NewRouteContext()
	rc.URLParams.Add("project_uuid", w.project)
	c := context.WithValue(context.WithValue(context.Background(), helpers.UserInfoContextKey, w.account(who)), chi.RouteCtxKey, rc)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, target, nil).WithContext(c))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// A board says which statuses its reader may move tasks into, where a rule
// says who (main's approvals check: the board offers no New task the server
// would refuse): the project's admins for ana, the admin, and not for ben, a
// member; named people for those it names alone, admins included. A status
// without a rule is not listed.
func TestABoardSaysWhereItsReaderMayMoveTasks(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana := w.account("ana")
	project := uuid.MustParse(w.project)
	for _, in := range []approvalsBusiness.RuleInput{
		{Status: "done", Who: "admins"},
		{Status: "inReview", Who: "people", People: []string{w.account("ben").UserPostgresInfo.Id.String()}},
	} {
		if _, err := approvalsBusiness.SaveRule(ctx, ana.UserPostgresInfo.Id, project, in); err != nil {
			t.Fatal(err)
		}
	}
	moveInto := func(who string) (int, map[string]any) {
		t.Helper()
		code, out := readProject(t, w, who, projectController.GetProjectTaskListForKanban, "/")
		data, _ := out["data"].(map[string]any)
		access, _ := data["project_access"].(map[string]any)
		may, _ := access["move_into"].(map[string]any)
		return code, may
	}
	if code, may := moveInto("ben"); code != http.StatusOK || may["done"] != false || may["inReview"] != true || len(may) != 2 {
		t.Errorf("ben, a member: %d %v", code, may)
	}
	if code, may := moveInto("ana"); code != http.StatusOK || may["done"] != true || may["inReview"] != false || len(may) != 2 {
		t.Errorf("ana, the project's admin: %d %v", code, may)
	}
}

// A held card says it's waiting for approval on its board (main's approvals
// check, gap 3): the board's read and a page of one of its columns carry each
// task's open request in their one batch, and nothing once it's answered.
// Someone outside the project gets no board to see it on.
func TestAHeldCardSaysItsWaitingOnItsBoard(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana, ben := w.account("ana"), w.account("ben")
	project := uuid.MustParse(w.project)
	if _, err := approvalsBusiness.SaveRule(ctx, ana.UserPostgresInfo.Id, project, approvalsBusiness.RuleInput{Status: "inReview", Who: "anyone", Approvers: []string{ana.UserPostgresInfo.Id.String()}, ApprovalsNeeded: 1}); err != nil {
		t.Fatal(err)
	}
	// ben's move of the task assigned to him becomes ana's request; the card stays in To do.
	_, _, err := taskWriter.Tasks.Apply(ctx, &ben, uuid.MustParse(w.mine), []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: json.RawMessage(`"inReview"`)}}, 0)
	r, ok := taskBusiness.RefusedStatus(err)
	if !ok || r.ApprovalID == "" {
		t.Fatalf("a move to In review becomes a request: %v", err)
	}
	// marks is each card's mark, by task, from lists of cards.
	marks := func(lists ...any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, l := range lists {
			cards, _ := l.([]any)
			for _, c := range cards {
				card, _ := c.(map[string]any)
				id, _ := card["task_uuid"].(string)
				mark, _ := card["task_waiting"].(map[string]any)
				out[id] = mark
			}
		}
		return out
	}
	board := func(who string) (int, map[string]map[string]any) {
		t.Helper()
		code, out := readProject(t, w, who, projectController.GetProjectTaskListForKanban, "/")
		data, _ := out["data"].(map[string]any)
		return code, marks(data["project_tasks_todo"], data["project_tasks_in_review"])
	}

	code, onBoard := board("ben")
	if mark := onBoard[w.mine]; code != http.StatusOK || mark == nil || mark["approval_id"] != r.ApprovalID || mark["to_status"] != "inReview" {
		t.Errorf("the held card on ben's board: %d %v", code, onBoard)
	}
	if _, listed := onBoard[w.task]; !listed || onBoard[w.task] != nil {
		t.Errorf("a card nobody asked about: listed %v, marked %v", listed, onBoard[w.task])
	}
	_, out := readProject(t, w, "ana", projectController.GetProjectBoardColumn, "/?status=todo")
	data, _ := out["data"].(map[string]any)
	if mark := marks(data["cards"])[w.mine]; mark == nil || mark["approval_id"] != r.ApprovalID {
		t.Errorf("the held card on a page of To do: %v", out)
	}
	// cy, in the team but not the project, gets no board.
	if code, _ := board("cy"); code != http.StatusForbidden {
		t.Errorf("cy reads the project's board: %d", code)
	}
	// Answered, the card is no longer marked.
	if _, err := approvalsBusiness.Decide(ctx, &ana, uuid.MustParse(w.mine), uuid.MustParse(r.ApprovalID), model.VerdictReject, ""); err != nil {
		t.Fatal(err)
	}
	if _, onBoard := board("ben"); onBoard[w.mine] != nil {
		t.Errorf("an answered request still marks the card: %v", onBoard[w.mine])
	}
}
