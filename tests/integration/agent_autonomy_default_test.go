//go:build integration

package integration_test

// A new agent asks first (ADMIN-16): ai_agents.autonomy's default is
// 'approval' after migration 589, and back to 'auto' after its down. Only the
// default moves; a row's own mode is never rewritten.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	agentBusiness "github.com/akashc777/OneCamp/business/AIAgent"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestAgentAutonomyDefaultMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		files, err := filepath.Glob("../../migrations/*_agent_autonomy_default." + dir + ".sql")
		if err != nil || len(files) != 1 {
			t.Fatalf("its %s file: %v %v", dir, files, err)
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	env := integration.SetupEnv(t)
	byDefault := func() string {
		t.Helper()
		var d string
		if err := env.PG.QueryRow(`SELECT column_default FROM information_schema.columns WHERE table_name = 'ai_agents' AND column_name = 'autonomy'`).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	owner := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, 'owner@example.test')`, owner); err != nil {
		t.Fatal(err)
	}
	// An agent that chose to act on its own keeps it through every step.
	kept := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by, autonomy) VALUES ($1, 'Lookout', $2, 'auto')`, kept, owner); err != nil {
		t.Fatal(err)
	}
	for i, step := range []struct{ sql, want string }{
		{"", "'approval'::text"}, {down, "'auto'::text"}, {up, "'approval'::text"}, {down, "'auto'::text"}, {up, "'approval'::text"},
	} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		if got := byDefault(); got != step.want {
			t.Fatalf("step %d: the default is %s, want %s", i, got, step.want)
		}
		var mode string
		if err := env.PG.QueryRow(`SELECT autonomy FROM ai_agents WHERE id = $1`, kept).Scan(&mode); err != nil || mode != "auto" {
			t.Fatalf("step %d: an existing agent's mode became %q (%v)", i, mode, err)
		}
	}
	// A row made without naming one asks first.
	fresh := uuid.New()
	var mode string
	if err := env.PG.QueryRow(`INSERT INTO ai_agents (id, name, created_by) VALUES ($1, 'New agent', $2) RETURNING autonomy`, fresh, owner).Scan(&mode); err != nil || mode != "approval" {
		t.Fatalf("a new row's mode: %q (%v)", mode, err)
	}
}

// The same through the saves people make, on a real database: an agent made
// without a mode asks first, and a save that sends none, or one that is no
// mode, keeps the mode the agent has instead of moving it.
func TestAnAgentSavedWithoutAModeAsksFirstAndKeepsIt(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	sponsor := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, 'sponsor@example.test')`, sponsor); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{{"uid": "_:sponsor", "user_uuid": sponsor.String(), "user_name": "sponsor"}})
	in := func(mode string) agentBusiness.AgentInput {
		return agentBusiness.AgentInput{
			Name:          "Lookout",
			Instructions:  "Answer questions.",
			TriggerType:   agentModel.TriggerMention,
			TriggerConfig: map[string]interface{}{},
			Scope:         map[string]interface{}{},
			IsActive:      true,
			MaxSteps:      4,
			Autonomy:      mode,
		}
	}
	stored := func(id uuid.UUID) string {
		t.Helper()
		var mode string
		if err := env.PG.QueryRow(`SELECT autonomy FROM ai_agents WHERE id = $1`, id).Scan(&mode); err != nil {
			t.Fatal(err)
		}
		return mode
	}
	for _, sent := range []string{"", "bogus"} {
		a, err := agentBusiness.CreateAgent(ctx, in(sent), sponsor)
		if err != nil {
			t.Fatalf("made with %q: %v", sent, err)
		}
		if a.Autonomy != agentModel.AutonomyApproval || stored(a.Id) != agentModel.AutonomyApproval {
			t.Fatalf("made with %q: %q, stored %q; want %q", sent, a.Autonomy, stored(a.Id), agentModel.AutonomyApproval)
		}
	}
	a, err := agentBusiness.CreateAgent(ctx, in(agentModel.AutonomyApproval), sponsor)
	if err != nil {
		t.Fatal(err)
	}
	owner := agentBusiness.Actor{UserID: sponsor}
	for i, step := range []struct{ sent, want string }{
		{agentModel.AutonomyPlan, agentModel.AutonomyPlan},
		{"", agentModel.AutonomyPlan},
		{agentModel.AutonomyAuto, agentModel.AutonomyAuto},
		{"", agentModel.AutonomyAuto},
		{"bogus", agentModel.AutonomyAuto},
		{agentModel.AutonomyApproval, agentModel.AutonomyApproval},
	} {
		got, err := agentBusiness.UpdateAgent(ctx, a.Id, in(step.sent), owner)
		if err != nil {
			t.Fatalf("save %d (sent %q): %v", i, step.sent, err)
		}
		if got.Autonomy != step.want || stored(a.Id) != step.want {
			t.Fatalf("save %d (sent %q): %q, stored %q; want %q", i, step.sent, got.Autonomy, stored(a.Id), step.want)
		}
	}
}
