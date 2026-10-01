//go:build integration
// +build integration

package integration_test

// A job that runs out of steps carries on in a new session from its saved
// conversation, a bounded number of times, and never without a lease or a
// conversation to continue.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestAgentTaskContinuesInBoundedSessions(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := uuid.MustParse(seedUser(t, env.PG))
	agentID := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by) VALUES ($1, 'Worker', $2)`, agentID, owner); err != nil {
		t.Fatal(err)
	}
	id, _, err := agentModel.EnqueueAgentTask(ctx, &agentModel.AgentTask{AgentId: agentID, SourceType: "assignment", SourceId: uuid.NewString(), Prompt: "do it", RunAsUserId: &owner})
	if err != nil {
		t.Fatal(err)
	}
	claim := func() uuid.UUID {
		t.Helper()
		job, err := agentModel.ClaimNextRunnable(ctx, time.Minute, 0)
		if err != nil || job == nil || job.Id != id {
			t.Fatalf("claim: %+v %v", job, err)
		}
		return *job.LeaseToken
	}
	const note = "continue where you left off"

	lease := claim()
	if ok, err := agentModel.ContinueAgentTaskSession(ctx, id, lease, note, 3, nil); err != nil || ok {
		t.Fatalf("with no saved conversation there is nothing to continue: %v %v", ok, err)
	}
	if err := agentModel.SaveAgentTaskMessages(ctx, id, lease, `[{"role":"user","content":"do it"}]`); err != nil {
		t.Fatal(err)
	}
	if ok, _ := agentModel.ContinueAgentTaskSession(ctx, id, uuid.New(), note, 3, nil); ok {
		t.Fatal("a worker without the lease must not move the job")
	}
	for session := 2; session <= 3; session++ {
		if ok, err := agentModel.ContinueAgentTaskSession(ctx, id, lease, note, 3, nil); err != nil || !ok {
			t.Fatalf("session %d: %v %v", session, ok, err)
		}
		lease = claim()
	}
	if ok, _ := agentModel.ContinueAgentTaskSession(ctx, id, lease, note, 3, nil); ok {
		t.Fatal("the third session is the last")
	}

	var sessions, attempt int
	var raw string
	if err := env.PG.QueryRow(`SELECT sessions, attempt, messages::text FROM ai_agent_tasks WHERE id=$1`, id).Scan(&sessions, &attempt, &raw); err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]string
	_ = json.Unmarshal([]byte(raw), &msgs)
	if sessions != 3 || len(msgs) != 3 || msgs[2]["content"] != note || msgs[2]["role"] != "user" {
		t.Fatalf("sessions=%d messages=%s", sessions, raw)
	}
	if attempt != 1 {
		t.Fatalf("continuing must not charge attempts, attempt=%d", attempt)
	}
}
