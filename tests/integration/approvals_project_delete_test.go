//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	approvalsBusiness "github.com/akashc777/OneCamp/business/Approvals"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
)

// Deleting a project closes its tasks' open requests, as deleting or
// archiving a task does (main's live check, 27 Oct: a deleted project's
// tasks still waited in Approvals): a held move and an ask close as moved
// on, and ana's waiting list drops them.
func TestADeletedProjectClosesItsTasksOpenRequests(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana, ben := w.account("ana"), w.account("ben")
	project := uuid.MustParse(w.project)
	if _, err := approvalsBusiness.SaveRule(ctx, ana.UserPostgresInfo.Id, project, approvalsBusiness.RuleInput{
		Status: "inReview", Who: "anyone", Approvers: []string{ana.UserPostgresInfo.Id.String()}, ApprovalsNeeded: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err := taskWriter.Tasks.Apply(ctx, &ben, uuid.MustParse(w.mine), []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: json.RawMessage(`"inReview"`)}}, 0)
	held, ok := taskBusiness.RefusedStatus(err)
	if !ok || held.ApprovalID == "" {
		t.Fatalf("ben's move to In review becomes a request: %v", err)
	}
	asked, err := approvalsBusiness.AskForTask(ctx, &ben, uuid.MustParse(w.task), approvalsBusiness.AskInput{Approvers: []string{ana.UserPostgresInfo.Id.String()}, Ask: "The layout"})
	if err != nil {
		t.Fatalf("ben asks about ana's task: %v", err)
	}

	// ana deletes the project, as the app's Delete project does.
	info, err := projectBusiness.GetDgraphProjectInfoAndTeamAdminFlag(ctx, w.project, ana.UserDgraphInfo.Uid)
	if err != nil || info == nil {
		t.Fatalf("read the project: %v", err)
	}
	if err := projectBusiness.ArchiveProjectByProjectUUID(ctx, project, info); err != nil {
		t.Fatalf("delete the project: %v", err)
	}

	for what, id := range map[string]uuid.UUID{"the held move": uuid.MustParse(held.ApprovalID), "the ask": asked.ID} {
		a, err := model.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if a.State != model.MovedOn {
			t.Errorf("%s about a task of the deleted project is %s", what, a.State)
		}
	}
	waiting, err := model.Waiting(ctx, ana.UserPostgresInfo.Id, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 0 {
		t.Errorf("ana still waits on %d requests about the deleted project's tasks", len(waiting))
	}
}
