//go:build integration

package business

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
)

// A workflow scoped to "any channel" runs on messages in channels its owner can
// read, and is skipped, before any step, in a private channel they aren't in;
// one scoped to "any project" likewise for task moves. The lookups are the real
// ones, against Postgres and Dgraph. Its steps are create_task, whose executor
// isn't registered in this test binary, so each run records that error and
// counts as a run without reaching anything else.
//
// Run: go test -tags=integration ./business/Workflow/ -run TestAWorkflowSkipsWhatItsOwnerCantSee -v
func TestAWorkflowSkipsWhatItsOwnerCantSee(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)

	owner, admin := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{owner, admin} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, id, id.String()[:8]+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	mine, theirs, open := uuid.NewString(), uuid.NewString(), uuid.NewString()
	ours, other := uuid.NewString(), uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "user_uuid": owner.String(), "user_name": "owner"},
		{"uid": "_:mine", "ch_uuid": mine, "ch_name": "mine", "ch_private": true,
			"ch_members": []map[string]any{{"uid": "_:owner"}}},
		{"uid": "_:theirs", "ch_uuid": theirs, "ch_name": "finance", "ch_private": true},
		{"uid": "_:open", "ch_uuid": open, "ch_name": "general", "ch_private": false},
		{"uid": "_:ours", "project_uuid": ours, "project_name": "ours",
			"project_members": []map[string]any{{"uid": "_:owner"}}},
		{"uid": "_:other", "project_uuid": other, "project_name": "payroll"},
	})

	copyAll, err := CreateWorkflow(ctx, WorkflowInput{
		Name:        "copy every message",
		IsActive:    true,
		TriggerType: workflowModel.TriggerMessagePosted,
		Actions:     []WorkflowAction{{Type: ActionCreateTask, ProjectID: ours}},
	}, owner)
	if err != nil {
		t.Fatal(err)
	}
	everyMove, err := CreateWorkflow(ctx, WorkflowInput{
		Name:        "every move",
		IsActive:    true,
		TriggerType: workflowModel.TriggerTaskStatusChanged,
		Actions:     []WorkflowAction{{Type: ActionCreateTask, ProjectID: ours, TaskName: "Follow up {task}"}},
	}, owner)
	if err != nil {
		t.Fatal(err)
	}
	runs := func(id uuid.UUID) int64 {
		t.Helper()
		w, err := workflowModel.GetWorkflowByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return w.RunCount
	}
	post := func(channel string) {
		handleEvent(ctx, "post.created", map[string]interface{}{
			"channel_id": channel, "text": "the salary bands for next year",
			"author_id": uuid.NewString(), "post_id": uuid.NewString(),
		})
	}
	move := func(project string) {
		handleEvent(ctx, "task.status_changed", map[string]interface{}{
			"task_id": uuid.NewString(), "task_name": "Cut Maya's pay", "project_id": project,
			"old_status": "todo", "new_status": "done", "updated_by_uuid": uuid.NewString(),
		})
	}

	for _, c := range []struct {
		name    string
		event   func()
		wf      uuid.UUID
		wantRun bool
	}{
		{"a private channel the owner is in", func() { post(mine) }, copyAll.Id, true},
		{"a private channel the owner isn't in", func() { post(theirs) }, copyAll.Id, false},
		{"a public channel", func() { post(open) }, copyAll.Id, true},
		{"a channel that doesn't exist", func() { post(uuid.NewString()) }, copyAll.Id, false},
		{"a move in the owner's project", func() { move(ours) }, everyMove.Id, true},
		{"a move in a project the owner isn't in", func() { move(other) }, everyMove.Id, false},
	} {
		before := runs(c.wf)
		c.event()
		if ran := runs(c.wf) > before; ran != c.wantRun {
			t.Errorf("%s: ran=%v, want %v", c.name, ran, c.wantRun)
		}
	}

	// Leaving a channel ends it: the check reads membership as it is now.
	raw, _ := json.Marshal(map[string]any{"uid": uids["mine"], "ch_members": []map[string]any{{"uid": uids["owner"]}}})
	if _, err := dgraphInit.DgraphClient.NewTxn().Mutate(ctx, &api.Mutation{DeleteJson: raw, CommitNow: true}); err != nil {
		t.Fatal(err)
	}
	before := runs(copyAll.Id)
	post(mine)
	if runs(copyAll.Id) != before {
		t.Error("a channel the owner left still runs the workflow")
	}

	// A scope the owner can't see is refused when it's saved, by its owner and
	// by an admin editing it alike.
	scoped := func(channel string) WorkflowInput {
		return WorkflowInput{Name: "scoped", IsActive: true, TriggerType: workflowModel.TriggerMessagePosted,
			ChannelID: channel, Actions: []WorkflowAction{{Type: ActionReply, Text: "seen"}}}
	}
	if _, err := CreateWorkflow(ctx, scoped(theirs), owner); err != errScopeChannel {
		t.Errorf("a workflow on a private channel its owner isn't in was saved: %v", err)
	}
	if _, err := CreateWorkflow(ctx, scoped(uuid.NewString()), owner); err != errScopeChannel {
		t.Errorf("a workflow on a channel that doesn't exist was saved: %v", err)
	}
	onOpen, err := CreateWorkflow(ctx, scoped(open), owner)
	if err != nil {
		t.Fatalf("a workflow on a public channel was refused: %v", err)
	}
	if _, err := UpdateWorkflow(ctx, onOpen.Id, scoped(theirs), Actor{UserID: admin, IsAdmin: true}); err != errScopeChannel {
		t.Errorf("an admin moved a member's workflow onto a channel the member can't read: %v", err)
	}
	if _, err := CreateWorkflow(ctx, WorkflowInput{
		Name: "payroll moves", IsActive: true, TriggerType: workflowModel.TriggerTaskStatusChanged,
		TriggerConfig: map[string]interface{}{"project_id": other, "to_status": "done"},
		Actions:       []WorkflowAction{{Type: ActionCreateTask, ProjectID: ours, TaskName: "x"}},
	}, owner); err != errScopeProject {
		t.Errorf("a workflow on moves in a project its owner isn't in was saved: %v", err)
	}
}
