//go:build integration
// +build integration

package integration_test

// A follow-up on a thread whose agent already opened a pull request pushes to
// that pull request instead of opening a second one. The job finds it through
// the audit rows its earlier jobs recorded (code_pr_runs.agent_task_id), and
// finds the original request through its first job.

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestCodePRFollowUpFindsItsThreadsPullRequest(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := uuid.MustParse(seedUser(t, env.PG))
	agentID := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by) VALUES ($1, 'Coder', $2)`, agentID, owner); err != nil {
		t.Fatal(err)
	}
	const sourceType, sourceID = "code_pr", "post:thread-1"

	// Nothing yet.
	if _, _, found, err := agentModel.FirstAgentTaskForSource(ctx, agentID, sourceType, sourceID); err != nil || found {
		t.Fatalf("no job yet: found=%v err=%v", found, err)
	}
	if pr, err := aiModels.LatestOpenedCodePRForSource(ctx, agentID, sourceType, sourceID); err != nil || pr != nil {
		t.Fatalf("no PR yet: %+v %v", pr, err)
	}

	enqueue := func(prompt string) uuid.UUID {
		t.Helper()
		id, created, err := agentModel.EnqueueAgentTask(ctx, &agentModel.AgentTask{AgentId: agentID, SourceType: sourceType, SourceId: sourceID, Prompt: prompt, RunAsUserId: &owner})
		if err != nil || !created {
			t.Fatalf("enqueue: %v %v", created, err)
		}
		// Settle it so the next job for the source can be enqueued.
		if _, err := env.PG.Exec(`UPDATE ai_agent_tasks SET state='done' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	record := func(jobID uuid.UUID, status, prURL, head string) {
		t.Helper()
		if _, err := aiModels.RecordCodePRRun(ctx, &aiModels.CodePRRun{ActorID: owner, AgentID: &agentID, AgentTaskID: &jobID,
			RepoOwner: "acme", RepoName: "svc", BaseBranch: "main", HeadBranch: head, Status: status, PRURL: prURL}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond) // created_at orders the rows
	}

	first := enqueue("Fix the padding on the login page")
	record(first, "ok", "https://github.com/acme/svc/pull/7", "onecamp-agent/fix-the-padding-1")
	followUp := enqueue("Also the margin")
	record(followUp, "no_green", "https://github.com/acme/svc/pull/7", "onecamp-agent/fix-the-padding-1")

	id, prompt, found, err := agentModel.FirstAgentTaskForSource(ctx, agentID, sourceType, sourceID)
	if err != nil || !found || id != first || prompt != "Fix the padding on the login page" {
		t.Fatalf("first job: %v %q %v %v", id, prompt, found, err)
	}
	pr, err := aiModels.LatestOpenedCodePRForSource(ctx, agentID, sourceType, sourceID)
	if err != nil || pr == nil || pr.PRURL != "https://github.com/acme/svc/pull/7" || pr.HeadBranch != "onecamp-agent/fix-the-padding-1" ||
		pr.RepoOwner != "acme" || pr.RepoName != "svc" || pr.BaseBranch != "main" {
		t.Fatalf("latest PR: %+v %v", pr, err)
	}

	// Another thread, or another agent, never sees this PR.
	if pr, _ := aiModels.LatestOpenedCodePRForSource(ctx, agentID, sourceType, "post:thread-2"); pr != nil {
		t.Fatalf("another thread's PR leaked: %+v", pr)
	}
	if pr, _ := aiModels.LatestOpenedCodePRForSource(ctx, uuid.New(), sourceType, sourceID); pr != nil {
		t.Fatalf("another agent's PR leaked: %+v", pr)
	}

	// A later PR on the same thread (the first was merged) is the one continued.
	third := enqueue("And the footer")
	record(third, "ok", "https://github.com/acme/svc/pull/9", "onecamp-agent/and-the-footer-3")
	if pr, _ := aiModels.LatestOpenedCodePRForSource(ctx, agentID, sourceType, sourceID); pr == nil || pr.PRURL != "https://github.com/acme/svc/pull/9" {
		t.Fatalf("want the newest PR: %+v", pr)
	}
}
