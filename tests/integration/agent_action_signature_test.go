//go:build integration
// +build integration

package integration_test

// A signed agent action survives the trip through Postgres (time zone and
// microsecond precision included) and still verifies; a row rewritten in the
// database does not.

import (
	"context"
	"testing"
	"time"

	agentBusiness "github.com/akashc777/OneCamp/business/AIAgent"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestAgentActionSignaturesSurviveTheDatabase(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	agent := uuid.New()
	for i := 0; i < 3; i++ {
		if _, err := agentBusiness.RecordActionIntentForTest(ctx, agent, "append_to_doc", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	r, err := agentBusiness.VerifyAgentActions(ctx, agent, 100)
	if err != nil || r.Checked != 3 || r.Valid != 3 {
		t.Fatalf("freshly recorded actions verify: %+v %v", r, err)
	}
	if _, err := env.PG.Exec(`UPDATE ai_agent_action_log SET tool_name = 'delete_channel' WHERE id = (SELECT id FROM ai_agent_action_log WHERE agent_id = $1 LIMIT 1)`, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO ai_agent_action_log (id, agent_id, tool_name, params_digest) VALUES ($1, $2, 'send_message', 'x')`, uuid.New(), agent); err != nil {
		t.Fatal(err)
	}
	r, _ = agentBusiness.VerifyAgentActions(ctx, agent, 100)
	if r.Checked != 4 || r.Valid != 2 || r.Invalid != 1 || r.Unsigned != 1 {
		t.Fatalf("a rewritten row fails and an inserted one is unsigned: %+v", r)
	}
}
