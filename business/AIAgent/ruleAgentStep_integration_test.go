//go:build integration

package business

import (
	"context"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A rule's question to an AI teammate, against Postgres: it queues as the
// teammate's work on the task, run as its sponsor and started by the rule's
// owner; asked again while that work is open, nothing new is queued; once it
// is done, the next question is new work.
//
// Run: go test -tags=integration ./business/AIAgent/ -run TestARuleQuestionQueuesOnceATaskAtATime -v
func TestARuleQuestionQueuesOnceATaskAtATime(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	sponsor, owner := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{sponsor, owner} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, u, u.String()[:8]+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	agent := &model.AiAgent{Id: uuid.New(), Name: "Triage Bot", CreatedBy: sponsor, IsActive: true}
	if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by, trigger_type, dm_able, ambient)
		VALUES ($1, $2, $3, 'mention', true, false)`, agent.Id, agent.Name, sponsor); err != nil {
		t.Fatal(err)
	}
	task := uuid.NewString()
	prompt := synthRuleQuestionPrompt("Is anything missing to finish this on time?", map[string]interface{}{"task_id": task, "name": "Fix the crash"})

	first, created, err := queueTaskWork(ctx, agent, task, prompt, &owner)
	if err != nil || !created {
		t.Fatalf("first question: created=%v err=%v", created, err)
	}
	var source, sourceID, state, saved string
	var runAs, by uuid.UUID
	if err := env.PG.QueryRow(`SELECT source_type, source_id, state, prompt, run_as_user_id, triggered_by FROM ai_agent_tasks WHERE id=$1`, first).
		Scan(&source, &sourceID, &state, &saved, &runAs, &by); err != nil {
		t.Fatal(err)
	}
	if source != model.TaskSourceAssignment || sourceID != task || state != "queued" || runAs != sponsor || by != owner {
		t.Errorf("job %s on %s is %s, run as %s, started by %s", source, sourceID, state, runAs, by)
	}
	if !strings.Contains(saved, "Is anything missing to finish this on time?") || !strings.Contains(saved, "task_uuid: "+task) {
		t.Errorf("the job's prompt lacks the question or the task:\n%s", saved)
	}

	again, created, err := queueTaskWork(ctx, agent, task, prompt, &owner)
	if err != nil || created || again != first {
		t.Fatalf("asked again while at work: id=%s created=%v err=%v, want the open job %s", again, created, err, first)
	}
	var open int
	if err := env.PG.QueryRow(`SELECT count(*) FROM ai_agent_tasks WHERE agent_id=$1 AND source_id=$2`, agent.Id, task).Scan(&open); err != nil || open != 1 {
		t.Fatalf("%d jobs on the task (err %v), want 1", open, err)
	}

	if _, err := env.PG.Exec(`UPDATE ai_agent_tasks SET state='done' WHERE id=$1`, first); err != nil {
		t.Fatal(err)
	}
	next, created, err := queueTaskWork(ctx, agent, task, prompt, &owner)
	if err != nil || !created || next == first {
		t.Fatalf("asked after the work was done: id=%s created=%v err=%v", next, created, err)
	}
}
