//go:build integration
// +build integration

package integration_test

// Pausing or deleting an agent stops its open jobs, and an agent whose sponsor
// has left the workspace is left out of what its triggers start.

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestSwitchingAnAgentOffStopsItsWork(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := uuid.MustParse(seedUser(t, env.PG))
	admin := uuid.MustParse(seedUser(t, env.PG))
	newAgent := func(sponsor uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by, trigger_type, dm_able, ambient)
			VALUES ($1, 'Agent', $2, 'mention', true, true)`, id, sponsor); err != nil {
			t.Fatal(err)
		}
		return id
	}

	t.Run("open jobs stop, finished ones are left alone", func(t *testing.T) {
		agent, other := newAgent(owner), newAgent(owner)
		job := func(agentID uuid.UUID, state string) uuid.UUID {
			t.Helper()
			id, _, err := agentModel.EnqueueAgentTask(ctx, &agentModel.AgentTask{AgentId: agentID, SourceType: "assignment", SourceId: uuid.NewString(), Prompt: "do it", RunAsUserId: &owner})
			if err != nil {
				t.Fatal(err)
			}
			if state == "running" {
				_, err = env.PG.Exec(`UPDATE ai_agent_tasks SET state='running', lease_token=$2, lease_expires_at=now()+interval '1 minute' WHERE id=$1`, id, uuid.New())
			} else if state != "queued" {
				_, err = env.PG.Exec(`UPDATE ai_agent_tasks SET state=$2 WHERE id=$1`, id, state)
			}
			if err != nil {
				t.Fatal(err)
			}
			return id
		}
		queued, running, parked, done := job(agent, "queued"), job(agent, "running"), job(agent, "awaiting_input"), job(agent, "done")
		elsewhere := job(other, "queued")

		ids, err := agentModel.RequestAgentTasksCancel(ctx, agent, admin)
		if err != nil {
			t.Fatal(err)
		}
		touched := map[uuid.UUID]bool{}
		for _, id := range ids {
			touched[id] = true
		}
		if len(ids) != 3 || !touched[queued] || !touched[running] || !touched[parked] {
			t.Fatalf("touched %v, want the queued, running and parked jobs", ids)
		}
		want := map[uuid.UUID]string{queued: "cancelled", parked: "cancelled", running: "running", done: "done", elsewhere: "queued"}
		for id, state := range want {
			var got string
			var by *uuid.UUID
			if err := env.PG.QueryRow(`SELECT state, cancel_requested_by FROM ai_agent_tasks WHERE id=$1`, id).Scan(&got, &by); err != nil {
				t.Fatal(err)
			}
			if got != state {
				t.Errorf("job %s is %s, want %s", id, got, state)
			}
			// The running job's worker settles it, naming who stopped it.
			if asked := by != nil && *by == admin; asked != touched[id] {
				t.Errorf("job %s: stop recorded=%v, want %v", id, asked, touched[id])
			}
		}
		if again, err := agentModel.RequestAgentTasksCancel(ctx, agent, owner); err != nil || len(again) != 1 || again[0] != running {
			t.Fatalf("asking again touches only the job still running: %v %v", again, err)
		}
		var by uuid.UUID
		if err := env.PG.QueryRow(`SELECT cancel_requested_by FROM ai_agent_tasks WHERE id=$1`, running).Scan(&by); err != nil || by != admin {
			t.Fatalf("a second request must not rewrite who stopped it: %v %v", by, err)
		}
	})

	t.Run("an agent whose sponsor left starts nothing", func(t *testing.T) {
		gone := uuid.MustParse(seedUser(t, env.PG))
		stays, orphan := newAgent(owner), newAgent(gone)
		if _, err := env.PG.Exec(`UPDATE users SET deleted_at=now() WHERE id=$1`, gone); err != nil {
			t.Fatal(err)
		}
		lists := map[string]func(context.Context) ([]*agentModel.AiAgent, error){
			"mention": func(c context.Context) ([]*agentModel.AiAgent, error) {
				return agentModel.ListActiveByTrigger(c, agentModel.TriggerMention)
			},
			"dm":      agentModel.ListDMable,
			"ambient": agentModel.ListAmbient,
		}
		for name, list := range lists {
			agents, err := list(ctx)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[uuid.UUID]bool{}
			for _, a := range agents {
				seen[a.Id] = true
			}
			if !seen[stays] || seen[orphan] {
				t.Errorf("%s: has the agent whose sponsor stays=%v, the one whose sponsor left=%v", name, seen[stays], seen[orphan])
			}
		}
	})
}
