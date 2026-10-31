//go:build integration
// +build integration

package integration_test

// Approvals on any item (package 9 R2b with H-5): a client's verdict counts
// on a request that names its link and moves the task through StatusGate;
// another link's verdict stays a review; the waiting list empties as the
// approver answers; asking for a task itself to be approved needs no move;
// and a client's link says what waits for its yes.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	approvalsBusiness "github.com/akashc777/OneCamp/business/Approvals"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAClientsApprovalOnTheirLinkMovesTheTask(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana, ben := w.account("ana"), w.account("ben")
	project := uuid.MustParse(w.project)
	mine := uuid.MustParse(w.mine)
	done := []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: json.RawMessage(`"done"`)}}

	// Client links need guest access on.
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	// Done wants ana and the client; the project has two client links, one
	// of them made after the request opens.
	first, err := guestBusiness.CreateResourceGrant(ctx, ana.UserPostgresInfo.Id, guestModel.ResourceProject, w.project, guestModel.CapabilityComment, 0, false)
	if err != nil {
		t.Fatalf("a client link: %v", err)
	}
	if _, err := approvalsBusiness.SaveRule(ctx, ana.UserPostgresInfo.Id, project, approvalsBusiness.RuleInput{Status: "done", Who: "anyone",
		Approvers: []string{ana.UserPostgresInfo.Id.String()}, ApprovalsNeeded: 1, Client: true}); err != nil {
		t.Fatal(err)
	}
	_, _, err = taskWriter.Tasks.Apply(ctx, &ben, mine, done, 0)
	r, ok := taskBusiness.RefusedStatus(err)
	if !ok || r.ApprovalID == "" || r.Why != "Sent for approval: it moves to Done once an approver and the client say yes." {
		t.Fatalf("a move that needs ana and the client: %v", err)
	}
	later, err := guestBusiness.CreateResourceGrant(ctx, ana.UserPostgresInfo.Id, guestModel.ResourceProject, w.project, guestModel.CapabilityComment, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := approvalsBusiness.WaitingFor(ctx, &ana)
	if err != nil || len(waiting) != 1 || waiting[0].ID != w.mine || waiting[0].ToName != "Done" {
		t.Fatalf("ana's waiting list: %+v %v", waiting, err)
	}

	// ana says yes: it still waits for the client.
	pending, err := model.PendingFor(ctx, mine)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := approvalsBusiness.Decide(ctx, &ana, mine, pending.ID, model.VerdictApprove, ""); err != nil || v.State != model.Pending {
		t.Fatalf("ana alone: %+v %v", v, err)
	}
	if waiting, _ := approvalsBusiness.WaitingFor(ctx, &ana); len(waiting) != 0 {
		t.Errorf("answered, ana still waits on %+v", waiting)
	}

	// A link the request doesn't name: a review, not a decision.
	other, err := guestModel.GetByID(ctx, later.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guestBusiness.ReviewAsGuest(ctx, other, w.mine, "Lee", "approved", ""); err != nil {
		t.Fatalf("another link's review: %v", err)
	}
	if p, err := model.PendingFor(ctx, mine); err != nil || len(p.Decisions) != 1 {
		t.Fatalf("another link's verdict counted: %+v %v", p, err)
	}

	// The named link's approval completes it: the task moves, as ben.
	named, err := guestModel.GetByID(ctx, first.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guestBusiness.ReviewAsGuest(ctx, named, w.mine, "Dana", "approved", "Lovely"); err != nil {
		t.Fatalf("the client's review: %v", err)
	}
	got, _ := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.mine, ana.UserDgraphInfo.Uid)
	if got.Status != "done" {
		t.Errorf("approved by ana and the client, the task is %s", got.Status)
	}
	latest, err := model.LatestFor(ctx, mine)
	if err != nil || latest.State != model.Approved || len(latest.Decisions) != 2 || !latest.Decisions[1].Client || latest.Decisions[1].Name != "Dana" {
		t.Errorf("the request: %+v %v", latest, err)
	}

	// Asking for the task itself to be approved needs no move.
	v, err := approvalsBusiness.AskForTask(ctx, &ben, mine, approvalsBusiness.AskInput{Approvers: []string{ana.UserPostgresInfo.Id.String()}, Ask: "The copy"})
	if err != nil || v.ToStatus != "" || v.Ask != "The copy" {
		t.Fatalf("an ask: %+v %v", v, err)
	}
	if v, err := approvalsBusiness.Decide(ctx, &ana, mine, v.ID, model.VerdictApprove, ""); err != nil || v.State != model.Approved {
		t.Fatalf("ana approves the task itself: %+v %v", v, err)
	}
	// Asking only the client, with no member to ask, opens a request too.
	if v, err := approvalsBusiness.AskForTask(ctx, &ben, mine, approvalsBusiness.AskInput{Client: true, Ask: "The layout"}); err != nil || !v.Client || len(v.Approvers) != 0 || v.Needed != 0 {
		t.Fatalf("an ask for only the client: %+v %v", v, err)
	}
}

// A client's link says what waits for its yes: the task, and its card on the
// board, carry the open request that names the link, with what is asked,
// where a held move goes, by when and who asked. A link the request doesn't
// name, and a link only to view, are asked nothing, and it goes once a
// client has answered, though an approver still hasn't.
func TestAClientLinkSaysWhatWaitsForItsApproval(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana, ben := w.account("ana"), w.account("ben")
	project := uuid.MustParse(w.project)
	mine := uuid.MustParse(w.mine)
	done := []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: json.RawMessage(`"done"`)}}
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	link := func(capability string) *guestModel.GuestGrant {
		t.Helper()
		made, err := guestBusiness.CreateResourceGrant(ctx, ana.UserPostgresInfo.Id, guestModel.ResourceProject, w.project, capability, 0, false)
		if err != nil {
			t.Fatalf("a client link: %v", err)
		}
		grant, err := guestModel.GetByID(ctx, made.GrantID)
		if err != nil {
			t.Fatal(err)
		}
		return grant
	}
	asked := func(grant *guestModel.GuestGrant) *guestBusiness.GuestAsk {
		t.Helper()
		v, err := guestBusiness.GetGuestTask(ctx, grant, w.mine)
		if err != nil {
			t.Fatalf("the client's task: %v", err)
		}
		return v.Waiting
	}

	// Done wants ana and the client, and ben's move waits for both.
	named, viewer := link(guestModel.CapabilityComment), link(guestModel.CapabilityView)
	if _, err := approvalsBusiness.SaveRule(ctx, ana.UserPostgresInfo.Id, project, approvalsBusiness.RuleInput{Status: "done", Who: "anyone",
		Approvers: []string{ana.UserPostgresInfo.Id.String()}, ApprovalsNeeded: 1, Client: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := taskWriter.Tasks.Apply(ctx, &ben, mine, done, 0); err == nil {
		t.Fatal("a move that waits for ana and the client went through")
	}
	later := link(guestModel.CapabilityComment)

	if a := asked(named); a == nil || a.ToLabel != "Done" || a.AskedBy != "ben" || a.Ask != "" || a.DueAt != nil || a.AskedAt.IsZero() {
		t.Fatalf("the named link is asked %+v", a)
	}
	page, err := guestBusiness.GetGuestProject(ctx, named)
	if err != nil {
		t.Fatal(err)
	}
	cards := 0
	for _, col := range page.Columns {
		for _, card := range col.Tasks {
			cards++
			if (card.ID == w.mine) != (card.Waiting != nil) {
				t.Errorf("on the board, %s is asked %+v", card.Name, card.Waiting)
			}
		}
	}
	if cards == 0 {
		t.Fatal("the client's board shows no tasks")
	}
	if a := asked(later); a != nil {
		t.Errorf("a link made after the ask is asked %+v", a)
	}
	if a := asked(viewer); a != nil {
		t.Errorf("a link only to view is asked %+v", a)
	}

	// The client says yes: nothing waits for them, while ana's yes still does.
	if _, err := guestBusiness.ReviewAsGuest(ctx, named, w.mine, "Dana", "approved", ""); err != nil {
		t.Fatalf("the client's review: %v", err)
	}
	if a := asked(named); a != nil {
		t.Errorf("answered, the link is still asked %+v", a)
	}
	pending, err := model.PendingFor(ctx, mine)
	if err != nil {
		t.Fatalf("ana's yes is still wanted: %v", err)
	}
	if v, err := approvalsBusiness.Decide(ctx, &ana, mine, pending.ID, model.VerdictApprove, ""); err != nil || v.State != model.Approved {
		t.Fatalf("ana approves: %+v %v", v, err)
	}

	// An ask in words, by a time, names every link that can answer.
	due := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	if _, err := approvalsBusiness.AskForTask(ctx, &ben, mine, approvalsBusiness.AskInput{Client: true, Ask: "The layout", DueAt: &due}); err != nil {
		t.Fatalf("an ask for the client: %v", err)
	}
	for what, grant := range map[string]*guestModel.GuestGrant{"the first link": named, "the later link": later} {
		if a := asked(grant); a == nil || a.Ask != "The layout" || a.ToLabel != "" || a.DueAt == nil || !a.DueAt.Equal(due) || a.AskedBy != "ben" {
			t.Errorf("%s is asked %+v", what, a)
		}
	}
	if a := asked(viewer); a != nil {
		t.Errorf("a link only to view is asked %+v", a)
	}
}

// approvalsMigration is the any-item migration, found by its name, as the
// integrator numbers it.
func approvalsMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_approvals_any_item."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one approvals_any_item %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The migration goes up, down and up again on Postgres 12, a task's open
// request surviving each way, and the API that runs before its restart
// (writing no resource_id) still opens one and answers it by user.
func TestApprovalsAnyItemMigrationUpDownUp(t *testing.T) {
	up, down := approvalsMigration(t, "up"), approvalsMigration(t, "down")
	if strings.Contains(up, "gen_random_uuid") {
		t.Fatal("ids are made in Go")
	}
	env := integration.SetupEnv(t)
	project, task, asker, approver := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	old := func(step int) uuid.UUID {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO approvals (id, project_id, task_id, to_status, requested_by, approvers, needed)
			VALUES ($1, $2, $3, 'done', $4, ARRAY[$5::uuid], 1)`, id, project, task, asker, approver); err != nil {
			t.Fatalf("step %d: the older API's request: %v", step, err)
		}
		if _, err := env.PG.Exec(`INSERT INTO approval_decisions (approval_id, user_id, verdict, comment) VALUES ($1, $2, 'approve', '')
			ON CONFLICT (approval_id, user_id) DO UPDATE SET verdict = EXCLUDED.verdict`, id, approver); err != nil {
			t.Fatalf("step %d: the older API's answer: %v", step, err)
		}
		if _, err := env.PG.Exec(`UPDATE approvals SET state = 'approved' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	old(0)
	for i, sql := range []string{down, up, down, up} {
		if _, err := env.PG.Exec(sql); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		old(i + 1)
	}
	var kind, id string
	if err := env.PG.QueryRow(`SELECT resource_kind, COALESCE(resource_id, task_id::text) FROM approvals ORDER BY created_at LIMIT 1`).Scan(&kind, &id); err != nil || kind != "task" || id != task.String() {
		t.Errorf("a task's request after the round trips: %s %s %v", kind, id, err)
	}
}

// Each reminder stage is claimed once: a day before the due time, then when
// it passes; the unanswered approvers come with the claim. A request asked
// less than a day before it's due skips the day-before stage (its ask was
// the notice) and is still told when it passes. A request with no member to
// ask, only the client, is kept like any other.
func TestADueRequestsRemindersAreClaimedOnce(t *testing.T) {
	env := integration.SetupEnv(t)
	integration.ConnectServerPostgres(t)
	ctx := context.Background()
	ann, bea := uuid.New(), uuid.New()
	at := time.Now().UTC().Truncate(time.Second)
	due, near := at.Add(36*time.Hour), at.Add(12*time.Hour)
	a, created, err := model.Open(ctx, &model.Approval{ResourceKind: model.KindTask, ResourceID: uuid.NewString(), ProjectID: uuid.New(),
		RequestedBy: uuid.New(), Approvers: []string{ann.String(), bea.String()}, Needed: 1, DueAt: &due})
	if err == nil && created {
		t.Fatal("a task's request without its task was taken")
	}
	task := uuid.New()
	a, created, err = model.Open(ctx, &model.Approval{ResourceKind: model.KindTask, ResourceID: task.String(), TaskID: task, ProjectID: uuid.New(),
		RequestedBy: uuid.New(), Approvers: []string{ann.String(), bea.String()}, Needed: 1, DueAt: &due})
	if err != nil || !created {
		t.Fatalf("open: %v %v", created, err)
	}
	clientOnly := uuid.New()
	c, created, err := model.Open(ctx, &model.Approval{ResourceKind: model.KindTask, ResourceID: clientOnly.String(), TaskID: clientOnly, ProjectID: uuid.New(),
		RequestedBy: uuid.New(), ClientGrants: []string{uuid.NewString()}, DueAt: &near})
	if err != nil || !created || len(c.Approvers) != 0 || !c.Client {
		t.Fatalf("a request for only the client, due in 12 hours: %+v %v %v", c, created, err)
	}
	if _, err := model.Decide(ctx, a.ID, ann, model.VerdictChanges, "Not yet"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`UPDATE approvals SET state = 'pending' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if soon, late, err := model.ClaimDue(ctx, at, 10); err != nil || len(soon)+len(late) != 0 {
		t.Fatalf("asked 12 hours before it's due: %+v %+v %v", soon, late, err)
	}
	soon, late, err := model.ClaimDue(ctx, at.Add(13*time.Hour), 10)
	if err != nil || len(soon) != 1 || soon[0].ID != a.ID || len(soon[0].Unanswered) != 1 || soon[0].Unanswered[0] != bea.String() ||
		len(late) != 1 || late[0].ID != c.ID {
		t.Fatalf("a day before one, past the other: %+v %+v %v", soon, late, err)
	}
	if soon, late, err = model.ClaimDue(ctx, at.Add(13*time.Hour+time.Minute), 10); err != nil || len(soon)+len(late) != 0 {
		t.Fatalf("claimed twice: %+v %+v %v", soon, late, err)
	}
	if soon, late, err = model.ClaimDue(ctx, due.Add(time.Minute), 10); err != nil || len(soon) != 0 || len(late) != 1 || late[0].ID != a.ID {
		t.Fatalf("past due: %+v %+v %v", soon, late, err)
	}
	if soon, late, err = model.ClaimDue(ctx, due.Add(2*time.Minute), 10); err != nil || len(soon)+len(late) != 0 {
		t.Fatalf("past due, claimed twice: %+v %+v %v", soon, late, err)
	}
}

// A task kept to some members (WA2) never waits for a client, and an
// approver it's kept from isn't asked. Each person acts in their own
// request, as in the app: ben is let into the task he's granted on.
func TestAPrivateTaskAsksNoClientAndNoOneItsKeptFrom(t *testing.T) {
	w := seedWorkWorld(t)
	asAna, ana := w.signedIn("ana")
	asBen, ben := w.signedIn("ben")
	project := uuid.MustParse(w.project)
	mine := uuid.MustParse(w.mine)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := guestBusiness.CreateResourceGrant(asAna, ana.UserPostgresInfo.Id, guestModel.ResourceProject, w.project, guestModel.CapabilityComment, 0, false); err != nil {
		t.Fatal(err)
	}
	restrict := func(id string, people ...uuid.UUID) {
		t.Helper()
		task, err := taskBusiness.GetDgraphBasicTaskInfo(asAna, id, ana.UserDgraphInfo.Uid)
		if err != nil {
			t.Fatal(err)
		}
		if err := workAccess.Restrict(asAna, task, ana.UserPostgresInfo.Id, people, time.Now()); err != nil {
			t.Fatalf("restrict: %v", err)
		}
	}
	// The task is kept to ben (and the project's admins, its assignee and its
	// creator), through Work Access as the app restricts one.
	restrict(w.mine, ben.UserPostgresInfo.Id)
	if r, err := approvalsBusiness.SaveRule(asAna, ana.UserPostgresInfo.Id, project, approvalsBusiness.RuleInput{Status: "done", Who: "anyone", Client: true}); err != nil || r == nil || !r.Client {
		t.Fatalf("a status that wants only the client: %+v %v", r, err)
	}
	done := []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: json.RawMessage(`"done"`)}}
	_, _, err := taskWriter.Tasks.Apply(asBen, &ben, mine, done, 0)
	if r, ok := taskBusiness.RefusedStatus(err); !ok || r.Why != "Moving it to Done needs the client's approval, and this task is private to some members." || r.ApprovalID != "" {
		t.Fatalf("a private task that wants the client: %v", err)
	}
	if _, err := approvalsBusiness.AskForTask(asBen, &ben, mine, approvalsBusiness.AskInput{Client: true}); !errors.Is(err, approvalsBusiness.ErrPrivateTask) {
		t.Errorf("asking the client about a private task: %v, want %v", err, approvalsBusiness.ErrPrivateTask)
	}

	// ana's own task, kept to its maker and the project's admins, is kept
	// from ben, so he can't be asked to approve it.
	restrict(w.task)
	want := ben.UserDgraphInfo.DisplayName() + " can't see this task, so can't approve it"
	if _, err := approvalsBusiness.AskForTask(asAna, &ana, uuid.MustParse(w.task), approvalsBusiness.AskInput{Approvers: []string{ben.UserPostgresInfo.Id.String()}}); err == nil || err.Error() != want {
		t.Errorf("asking someone the task is kept from: %v, want %q", err, want)
	}
}

// A task made private after its move was sent to the client is gone from
// the client's link: their answer is refused there, and the request goes on
// waiting until a member withdraws it.
func TestAClientCantAnswerForATaskMadePrivateAfterTheAsk(t *testing.T) {
	w := seedWorkWorld(t)
	asAna, ana := w.signedIn("ana")
	asBen, ben := w.signedIn("ben")
	project := uuid.MustParse(w.project)
	mine := uuid.MustParse(w.mine)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	link, err := guestBusiness.CreateResourceGrant(asAna, ana.UserPostgresInfo.Id, guestModel.ResourceProject, w.project, guestModel.CapabilityComment, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := approvalsBusiness.SaveRule(asAna, ana.UserPostgresInfo.Id, project, approvalsBusiness.RuleInput{Status: "done", Who: "anyone", Client: true}); err != nil || r == nil || !r.Client {
		t.Fatalf("a status that wants only the client: %+v %v", r, err)
	}
	done := []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: json.RawMessage(`"done"`)}}
	_, _, err = taskWriter.Tasks.Apply(asBen, &ben, mine, done, 0)
	if r, ok := taskBusiness.RefusedStatus(err); !ok || r.ApprovalID == "" {
		t.Fatalf("a move sent to the client: %v", err)
	}
	task, err := taskBusiness.GetDgraphBasicTaskInfo(asAna, w.mine, ana.UserDgraphInfo.Uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := workAccess.Restrict(asAna, task, ana.UserPostgresInfo.Id, []uuid.UUID{ben.UserPostgresInfo.Id}, time.Now()); err != nil {
		t.Fatalf("restrict: %v", err)
	}
	grant, err := guestModel.GetByID(asAna, link.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	// The client's request carries no person: it reads as nobody.
	if _, err := guestBusiness.ReviewAsGuest(taskread.Install(context.Background()), grant, w.mine, "Dana", "approved", ""); !errors.Is(err, guestBusiness.ErrNotFound) {
		t.Fatalf("the client's answer on a private task: %v, want %v", err, guestBusiness.ErrNotFound)
	}
	if p, err := model.PendingFor(asAna, mine); err != nil || len(p.Decisions) != 0 {
		t.Errorf("the request after the refused answer: %+v %v", p, err)
	}
	if got, err := taskBusiness.GetDgraphBasicTaskInfo(asBen, w.mine, ben.UserDgraphInfo.Uid); err != nil || got.Status == "done" {
		t.Errorf("the task after the refused answer: %+v %v", got, err)
	}
}

// An approver who has left isn't asked, and the rule isn't lowered for them:
// a status that wants two yeses holds the move while one approver is left,
// and one that wants a single yes goes to the approver still here. Their row
// read as gone is someone who has left (ActorByUUID's ErrNoActor), never a
// failed read that would hold every move.
func TestAnApproverWhoHasLeftIsntAsked(t *testing.T) {
	w := seedWorkWorld(t)
	asAna, ana := w.signedIn("ana")
	project, task := uuid.MustParse(w.project), uuid.MustParse(w.task)
	rule := approvalsBusiness.RuleInput{Status: "done", Who: "anyone",
		Approvers: []string{ana.UserPostgresInfo.Id.String(), w.people["ben"].String()}, ApprovalsNeeded: 2}
	if _, err := approvalsBusiness.SaveRule(asAna, ana.UserPostgresInfo.Id, project, rule); err != nil {
		t.Fatal(err)
	}
	w.exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1`, w.people["ben"])
	done := []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: json.RawMessage(`"done"`)}}
	_, _, err := taskWriter.Tasks.Apply(asAna, &ana, task, done, 0)
	if r, ok := taskBusiness.RefusedStatus(err); !ok || r.ApprovalID != "" || r.Why != "Moving it to Done needs 2 approvals, and only 1 of its approvers is still in the workspace." {
		t.Fatalf("two yeses wanted, one approver left: %v", err)
	}
	rule.ApprovalsNeeded = 1
	if _, err := approvalsBusiness.SaveRule(asAna, ana.UserPostgresInfo.Id, project, rule); err != nil {
		t.Fatal(err)
	}
	_, _, err = taskWriter.Tasks.Apply(asAna, &ana, task, done, 0)
	if r, ok := taskBusiness.RefusedStatus(err); !ok || r.Why != "Sent for approval: it moves to Done once it's approved." {
		t.Fatalf("one yes wanted, one approver left: %v", err)
	}
	if p, err := model.PendingFor(asAna, task); err != nil || len(p.Approvers) != 1 || p.Approvers[0] != ana.UserPostgresInfo.Id.String() || p.Needed != 1 {
		t.Errorf("the request: %+v %v", p, err)
	}
}
