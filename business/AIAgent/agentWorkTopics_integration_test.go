//go:build integration

package business

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A4: every member of a project hears its topic, so an agent's work on a task
// only a few may see never goes there; the people's own topics still hear it.
// With the real reads: the task's project and its restriction, from Postgres.
// Without ProjectTopicHears, the project's topic heard the task's id and the
// agent's state.
//
// Run: go test -tags=integration ./business/AIAgent/ -run TestAnAgentsWorkOnARestrictedTaskStaysOffItsProjectsTopic -v
func TestAnAgentsWorkOnARestrictedTaskStaysOffItsProjectsTopic(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}

	owner, team, project, open, secret := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, 'owner@example.test', 'owner', 'owner', NOW(), NOW())`, []any{owner}},
		{`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Office', $2)`, []any{team, owner}},
		{`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Payroll', $2, $3)`, []any{project, team, owner}},
		{`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $3, $4), ($2, $3, $4)`, []any{open, secret, project, owner}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := restrictionModel.Restrict(ctx, secret, project, owner, nil, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	onTask, err := EncodeSurface(Surface{Kind: SurfaceTask})
	if err != nil {
		t.Fatal(err)
	}
	topicsFor := func(task uuid.UUID) []string {
		return agentWorkTopics(ctx, &model.AgentTask{Id: uuid.New(), Surface: onTask, SourceId: "task:" + task.String(), TriggeredBy: &owner}, uuid.Nil)
	}
	projectTopic := helpers.GetMqttTopicForProjectMessage(project.String())
	theirs := helpers.GetMqttTopicForUserActivity(owner.String())
	if got := topicsFor(open); !slices.Contains(got, projectTopic) || !slices.Contains(got, theirs) {
		t.Fatalf("an open task's agent work goes to %v, want its project's topic and the person's", got)
	}
	if got := topicsFor(secret); slices.Contains(got, projectTopic) || !slices.Contains(got, theirs) {
		t.Fatalf("a restricted task's agent work goes to %v, want the person's topic and not its project's", got)
	}
}
