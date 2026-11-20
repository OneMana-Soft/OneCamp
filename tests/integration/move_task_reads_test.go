//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	approvalsBusiness "github.com/akashc777/OneCamp/business/Approvals"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// H4W (backend optimisation): a status change reads a task's GitHub link and
// its project's cycle schedule through the write it makes anyway, and reads
// them again only where they are there. A task that links an issue still
// queues its status for GitHub, and a start in a scheduled project still
// reads the schedule its cycle join needs.
func TestAStatusChangeReadsWhatItsTaskAndProjectHave(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	integration.AcceptingOpenSearch(t)
	w.exec(`UPDATE tasks SET github_issue_url = 'https://github.com/acme/web/issues/7' WHERE id = $1`, w.task)
	if _, err := cycleModel.SetSchedule(context.Background(), cycleModel.Schedule{ProjectUUID: uuid.MustParse(w.project),
		Weeks: 2, Upcoming: 1, AddStarted: true, Anchor: time.Now().UTC().Truncate(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	calls := integration.CountStoreCalls(t, w.env, w.dg.Endpoint)
	code, out := measuredPost(w, calls, "ana", taskController.UpdateTaskStatus, map[string]string{"task_uuid": w.task, "task_status": "inProgress", "task_project_uuid": w.project})
	if code != http.StatusOK {
		t.Fatalf("the status change: %d %s", code, out)
	}
	t.Logf("a linked task in a scheduled project: %s", callsSaid(calls))
	if n := calls.Sent("SELECT github_issue_url"); n != 1 {
		t.Errorf("a linked task's status change read its GitHub link %d times, want once (its sync)", n)
	}
	if n := calls.Sent("SELECT project_uuid, weeks"); n != 1 {
		t.Errorf("a start in a scheduled project read its schedule %d times, want once (its cycle join)", n)
	}
	// The sync is queued on its own goroutine (setStatus), which may still be
	// writing when the call answers.
	var queued int
	eventually(t, "a linked task's status change to queue its status sync", func() bool {
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM github_sync_queue WHERE task_id = $1 AND sync_type = 'status'`, w.task).Scan(&queued); err != nil {
			t.Fatal(err)
		}
		return queued > 0
	})
	if queued != 1 {
		t.Errorf("a linked task's status change queued %d status syncs, want 1", queued)
	}
}

// A drag that goes on without its task's open request still closes it as
// moved on (H4W): the gate reads the task's open request with its rules, in
// one round trip, and only a move that found none sends no close. An
// approved move finds its own request closed already.
func TestABoardDragClosesTheRequestItGoesOnWithout(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	integration.AcceptingOpenSearch(t)
	ctx := context.Background()
	ana, ben := w.account("ana"), w.account("ben")
	if _, err := approvalsBusiness.SaveRule(ctx, ana.UserPostgresInfo.Id, uuid.MustParse(w.project), approvalsBusiness.RuleInput{Status: "inReview", Who: "anyone",
		Approvers: []string{ana.UserPostgresInfo.Id.String()}, ApprovalsNeeded: 1}); err != nil {
		t.Fatal(err)
	}
	calls := integration.CountStoreCalls(t, w.env, w.dg.Endpoint)
	mine := uuid.MustParse(w.mine)
	measured := func(what string, f func()) {
		t.Helper()
		integration.Settle(t)
		calls.Reset()
		f()
		integration.Settle(t)
		t.Logf("%s: %s", what, callsSaid(calls))
	}
	// A held move costs no more than it did before H4W (base: 14 round trips
	// for the first, 13 for the next, 3 graph calls each).
	ask := func(trips int) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		measured("a held move", func() {
			_, _, err := taskWriter.Tasks.Apply(calls.Context(ctx), &ben, mine, []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: json.RawMessage(`"inReview"`)}}, 0)
			r, ok := taskBusiness.RefusedStatus(err)
			if !ok || r.ApprovalID == "" {
				t.Fatalf("a move to In review becomes a request: %v", err)
			}
			id = uuid.MustParse(r.ApprovalID)
		})
		if n, g := calls.Trips(), calls.Graph(); n > trips || g > 3 {
			t.Errorf("a held move took %d round trips and %d graph calls, want at most %d and 3", n, g, trips)
		}
		return id
	}
	// The gate's read of a move is one round trip, the rule and the open
	// request both: a read the batch couldn't scan ran again alone after it.
	gateRead := func(wantOpen bool) {
		t.Helper()
		integration.Settle(t)
		calls.Reset()
		rules, open, err := model.RulesAndOpen(calls.Context(ctx), uuid.MustParse(w.project), mine, []string{"inReview"})
		if err != nil || len(rules) != 1 || len(rules[0].Approvers) != 1 || rules[0].Approvers[0] != ana.UserPostgresInfo.Id.String() || open != wantOpen {
			t.Fatalf("the gate's read: %d rules, open %v, %v; want In review's rule with ana approving, open %v", len(rules), open, err, wantOpen)
		}
		if n := calls.Trips(); n != 1 {
			t.Errorf("the gate's read took %d round trips, want 1: %s", n, callsSaid(calls))
		}
	}
	state := func(id uuid.UUID) string {
		t.Helper()
		a, err := model.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return a.State
	}
	drag := func(to string) {
		t.Helper()
		code, out := measuredPost(w, calls, "ben", taskController.MoveTask, map[string]string{"task_uuid": w.mine, "task_status": to, "before_task_uuid": "", "after_task_uuid": ""})
		if code != http.StatusOK {
			t.Fatalf("the drag to %s: %d %s", to, code, out)
		}
	}

	gateRead(false)
	first := ask(14)
	gateRead(true)
	drag("inProgress")
	t.Logf("a drag that went on without a request: %s", callsSaid(calls))
	if got := state(first); got != model.MovedOn {
		t.Errorf("the task was dragged on, and its request is %s, want %s", got, model.MovedOn)
	}
	if n := calls.Sent("UPDATE approvals"); n != 1 {
		t.Errorf("the drag closed its task's open request %d times, want once", n)
	}

	second := ask(13)
	measured("an approved move", func() {
		if _, err := approvalsBusiness.Decide(calls.Context(ctx), &ana, mine, second, model.VerdictApprove, ""); err != nil {
			t.Fatalf("ana approves: %v", err)
		}
	})
	if got := state(second); got != model.Approved {
		t.Errorf("an approved request is %s, want %s", got, model.Approved)
	}

	drag("todo")
	if n := calls.Sent("UPDATE approvals"); n != 0 {
		t.Errorf("a drag of a task with no open request sent %d closes, want none", n)
	}
}
