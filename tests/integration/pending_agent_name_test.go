//go:build integration
// +build integration

package integration_test

// An approval says who is asking. The card (For you, the tray, Home) reads
// "Release Captain wants to…", so the open approvals carry their agent's name,
// read once for the whole list. An agent deleted since it asked is unnamed
// rather than named by a row nobody can open any more.

import (
	"context"
	"testing"
	"time"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestOpenApprovalsNameTheirAgent(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := uuid.MustParse(seedUser(t, env.PG))
	other := uuid.MustParse(seedUser(t, env.PG))

	captain, gone := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by) VALUES ($1, 'Release Captain', $2)`, captain, owner); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by, deleted_at) VALUES ($1, 'Old helper', $2, NOW())`, gone, owner); err != nil {
		t.Fatalf("seed deleted agent: %v", err)
	}

	propose := func(who uuid.UUID, desc string, agent *uuid.UUID) uuid.UUID {
		t.Helper()
		a, err := pendingModels.CreatePendingAction(ctx, who, pendingModels.SurfaceAssistant, "", "update_task_status",
			map[string]string{"task_uuid": uuid.NewString()}, desc, "", time.Now().Add(time.Hour),
			pendingModels.Attribution{AgentID: agent}, pendingModels.Binding{})
		if err != nil || a == nil {
			t.Fatalf("create: %v", err)
		}
		return a.Id
	}
	byCaptain := propose(owner, "Mark the launch task done", &captain)
	byAssistant := propose(owner, "Mark the docs task done", nil)
	byGone := propose(owner, "Mark the old task done", &gone)
	propose(other, "Someone else's approval", &captain)

	actions, err := aiBusiness.ListOpenPendingActions(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uuid.UUID]string{byCaptain: "Release Captain", byAssistant: "", byGone: ""}
	if len(actions) != len(want) {
		t.Fatalf("listed %d approvals, want the owner's %d only", len(actions), len(want))
	}
	for _, a := range actions {
		name, ok := want[a.Id]
		if !ok {
			t.Fatalf("listed %s, which is not the owner's", a.Id)
		}
		if a.AgentName != name {
			t.Fatalf("%q named %q, want %q", a.Description, a.AgentName, name)
		}
	}
}

// A new approval is named as it is made, on real Postgres: the single action
// and the agent's plan each come back (and go out live and pushed) with the
// agent's name, from the same rows the list reads.
func TestNewApprovalsAreNamedWhenMade(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := uuid.MustParse(seedUser(t, env.PG))
	captain := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by) VALUES ($1, 'Release Captain', $2)`, captain, owner); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	by := pendingModels.Attribution{AgentID: &captain}
	params := map[string]string{"task_uuid": uuid.NewString(), "status": "done"}

	one, err := aiBusiness.CreatePendingAction(ctx, owner, "agent", captain.String(), "update_task_status", params, "Mark the launch task done", "", by)
	if err != nil || one == nil {
		t.Fatalf("create action: %v", err)
	}
	plan, err := aiBusiness.CreatePlanAction(ctx, owner, "agent", captain.String(),
		[]aiBusiness.PlanStep{{ToolName: "update_task_status", Params: params, Description: "Mark the launch task done"}},
		"Release Captain proposes 1 step", by)
	if err != nil || plan == nil {
		t.Fatalf("create plan: %v", err)
	}
	for _, a := range []*pendingModels.PendingAction{one, plan} {
		if a.AgentName != "Release Captain" {
			t.Fatalf("%s came back named %q", a.ToolName, a.AgentName)
		}
	}
}
