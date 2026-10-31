//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/lib/pq"

	approvalsBusiness "github.com/akashc777/OneCamp/business/Approvals"
	archiveBusiness "github.com/akashc777/OneCamp/business/Archive"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
)

// An archive run closes the open requests of the tasks it takes, as deleting
// a task does (main's approvals check, gap 2: deleting or archiving the task
// closes it too): a held move on one and an ask about another close as moved
// on, and ana's waiting list drops them. A task the run leaves keeps its own.
func TestAnArchiveRunClosesItsTasksOpenRequests(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana, ben := w.account("ana"), w.account("ben")
	if _, err := approvalsBusiness.SaveRule(ctx, ana.UserPostgresInfo.Id, uuid.MustParse(w.project), approvalsBusiness.RuleInput{
		Status: "inReview", Who: "anyone", Approvers: []string{ana.UserPostgresInfo.Id.String()}, ApprovalsNeeded: 1,
	}); err != nil {
		t.Fatal(err)
	}
	held := func(task string) uuid.UUID {
		t.Helper()
		_, _, err := taskWriter.Tasks.Apply(ctx, &ben, uuid.MustParse(task), []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: json.RawMessage(`"inReview"`)}}, 0)
		r, ok := taskBusiness.RefusedStatus(err)
		if !ok || r.ApprovalID == "" {
			t.Fatalf("ben's move of %s to In review becomes a request: %v", task, err)
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

	// ben's move of the task ana gave him waits for her, and so does his ask
	// about her own task; both tasks are a year old. His newer task's move
	// waits too.
	move := held(w.mine)
	asked, err := approvalsBusiness.AskForTask(ctx, &ben, uuid.MustParse(w.task), approvalsBusiness.AskInput{Approvers: []string{ana.UserPostgresInfo.Id.String()}, Ask: "The layout"})
	if err != nil {
		t.Fatalf("ben asks about ana's task: %v", err)
	}
	newer := held(w.assigned)
	if _, err := w.env.PG.Exec(`UPDATE tasks SET created_at = NOW() - interval '1 year' WHERE id = ANY($1::uuid[])`, pq.Array([]string{w.mine, w.task})); err != nil {
		t.Fatal(err)
	}

	// The workspace archives every task older than 30 days.
	days, completedOnly := 30, false
	if err := archiveBusiness.UpdateArchivePolicy(ctx, "tasks", archiveBusiness.PolicyUpdateInput{RetentionDays: &days, ArchiveCompletedTasks: &completedOnly}); err != nil {
		t.Fatal(err)
	}
	job, err := archiveBusiness.RunArchiveJob(ctx, "tasks", nil)
	if err != nil {
		t.Fatalf("start the archive run: %v", err)
	}
	var status string
	var archived int
	eventually(t, "the archive run finishes", func() bool {
		_ = w.env.PG.QueryRow(`SELECT status, items_archived FROM archive_jobs WHERE id = $1`, job).Scan(&status, &archived)
		return status != "running"
	})
	if status != "completed" || archived != 2 {
		t.Fatalf("the archive run: %s, %d archived", status, archived)
	}

	if got := state(move); got != model.MovedOn {
		t.Errorf("the archived task's held move is %s", got)
	}
	if got := state(asked.ID); got != model.MovedOn {
		t.Errorf("the ask about the archived task is %s", got)
	}
	if got := state(newer); got != model.Pending {
		t.Errorf("the request of a task the run left is %s", got)
	}
	waiting, err := model.Waiting(ctx, ana.UserPostgresInfo.Id, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 1 || waiting[0].ID != newer {
		t.Errorf("ana waits on %d requests, want only the newer task's", len(waiting))
	}
}
