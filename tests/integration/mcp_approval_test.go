//go:build integration
// +build integration

package integration_test

// The MCP approval path, against a real ai_pending_actions table.
//
// WHY THIS TEST HAD TO EXIST. The approval path was wired with source-reading ratchets
// and a unit test on its status mapping — which prove the code is SHAPED correctly and
// say nothing about whether it works. CheckExistingWrite had never read a real row and
// RequestApproval had never written one. For the mechanism that stands between an
// external agent and a destructive change to a customer's workspace, "the ordering
// compiles correctly" is not evidence.
//
// It exercises the two functions that touch the store, composed with the policy that
// decides — which together ARE the approval path. The transport's ordering is covered
// separately, because that genuinely is a property of the source.
//
// The case worth the most here is the concurrency one: a row mid-execution must block a
// retry. That is the reading a careless mapping gets wrong, because the work genuinely
// has not finished yet, and it is the case that produces a duplicate write in
// production rather than a test failure.
//
// Run: go test -tags=integration ./tests/integration/ -run TestMCPApproval -v

import (
	"context"
	"database/sql"
	"testing"

	mcpBusiness "github.com/akashc777/OneCamp/business/MCPServer"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// destructiveSpec is a tool that needs approval.
//
// Named after a real registry tool because CreatePendingAction validates the proposal
// against the same registry approval will use — a proposal that could never run is
// refused at creation, which is correct and means a synthetic name would not survive.
// The BEHAVIOUR is the test's own: declaring it destructive is what puts it on the
// approval path, and no destructive tool is bridged yet on purpose.
func destructiveSpec() *mcpBusiness.ToolSpec {
	return &mcpBusiness.ToolSpec{
		Name:          "update_task_status",
		Description:   "test spec",
		InputSchema:   []byte(`{"type":"object"}`),
		RequiredScope: "tasks:write",
		Behaviour: mcpBusiness.ToolBehaviour{
			ReadOnly:    false,
			Destructive: true,
			Idempotent:  false,
		},
		// A destructive tool must carry a key: it is how a retry finds the card the
		// first attempt raised instead of stacking a second one.
		IdempotencyKey: func(args map[string]any) (string, error) {
			return "task-" + args["task_uuid"].(string), nil
		},
		Resource: func(args map[string]any) (mcpBusiness.ResourceRef, error) {
			return mcpBusiness.ResourceRef{
				Kind:   mcpBusiness.ResourceTask,
				ID:     args["task_uuid"].(string),
				Access: mcpBusiness.AccessWrite,
			}, nil
		},
		Handler: func(context.Context, mcpBusiness.ToolCallContext) (any, error) {
			return nil, nil
		},
	}
}

func TestMCPApprovalPathAgainstRealPostgres(t *testing.T) {
	env := integration.SetupEnv(t)

	// Point the project's global connection at the container, so the model layer under
	// test talks to this database rather than needing to be threaded a handle.
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatalf("connect the project's pool to the test database: %v", err)
	}

	ctx := context.Background()
	principal := seedUser(t, env.PG)
	taskID := uuid.NewString()

	spec := destructiveSpec()
	call := mcpBusiness.ToolCallContext{
		PrincipalUserID: principal,
		Args:            map[string]any{"task_uuid": taskID, "status": "done"},
		Resource: mcpBusiness.ResourceRef{
			Kind:   mcpBusiness.ResourceTask,
			ID:     taskID,
			Access: mcpBusiness.AccessWrite,
		},
	}

	// The key the policy will derive. Computed once so every stage below refers to the
	// same logical write, which is the thing being tracked.
	key, err := mcpBusiness.DeriveIdempotencyKey(spec, call)
	if err != nil {
		t.Fatalf("derive idempotency key: %v", err)
	}
	if key == "" {
		t.Fatal("a destructive non-idempotent tool must derive a key")
	}

	// STAGE 1: nothing has been asked yet.
	t.Run("an unseen call needs approval and finds no prior state", func(t *testing.T) {
		existing, err := mcpBusiness.CheckExistingWrite(ctx, key)
		if err != nil {
			t.Fatalf("check existing: %v", err)
		}
		if existing.State != mcpBusiness.ExistingNone {
			t.Fatalf("state = %q, want %q for a key never used", existing.State, mcpBusiness.ExistingNone)
		}

		plan := mcpBusiness.PlanWrite(ctx, spec, call, "test-client", mcpBusiness.CheckExistingWrite)
		if plan.Outcome != mcpBusiness.WriteNeedsApproval {
			t.Fatalf("outcome = %q, want %q; a destructive write must not proceed unasked",
				plan.Outcome, mcpBusiness.WriteNeedsApproval)
		}
	})

	// STAGE 2: raise the card.
	var cardID string
	t.Run("requesting approval creates one card", func(t *testing.T) {
		id, err := mcpBusiness.RequestApproval(ctx, spec, call, "test-client", key)
		if err != nil {
			t.Fatalf("request approval: %v", err)
		}
		if id == "" {
			t.Fatal("no pending action id returned, so nobody could be pointed at the card")
		}
		cardID = id

		// Attributed to the PRINCIPAL, which is what makes the card answerable — a
		// reviewer can ask that person why. Attributing it to the agent would produce a
		// card requested by software with nobody to ask.
		var requestedBy string
		if err := env.PG.QueryRow(
			`SELECT requested_by::text FROM ai_pending_actions WHERE id = $1`, cardID,
		).Scan(&requestedBy); err != nil {
			t.Fatalf("read the card back: %v", err)
		}
		if requestedBy != principal {
			t.Errorf("requested_by = %s, want the principal %s", requestedBy, principal)
		}
	})

	// STAGE 3: the property that matters for a retrying client.
	t.Run("a retry finds the SAME card instead of stacking another", func(t *testing.T) {
		existing, err := mcpBusiness.CheckExistingWrite(ctx, key)
		if err != nil {
			t.Fatalf("check existing: %v", err)
		}
		if existing.State != mcpBusiness.ExistingAwaitingApproval {
			t.Fatalf("state = %q, want %q", existing.State, mcpBusiness.ExistingAwaitingApproval)
		}
		if existing.PendingActionID != cardID {
			t.Fatalf("pending id = %s, want the original card %s", existing.PendingActionID, cardID)
		}

		// And the plan carries the id through, so the transport can tell the client
		// which card to wait on rather than raising a new one.
		plan := mcpBusiness.PlanWrite(ctx, spec, call, "test-client", mcpBusiness.CheckExistingWrite)
		if plan.Outcome != mcpBusiness.WriteNeedsApproval || plan.PendingActionID != cardID {
			t.Fatalf("plan = %+v, want needs_approval carrying card %s", plan, cardID)
		}

		// The store's unique index is the real guarantee. Asserted directly, because a
		// second card would mean a human being asked the same question twice.
		var cards int
		if err := env.PG.QueryRow(
			`SELECT count(*) FROM ai_pending_actions WHERE idempotency_key = $1`, key,
		).Scan(&cards); err != nil {
			t.Fatalf("count cards: %v", err)
		}
		if cards != 1 {
			t.Fatalf("%d cards exist for one logical write; a retry must not ask a human "+
				"the same question again", cards)
		}
	})

	// STAGE 4: every terminal state, in the order a real card moves through them.
	//
	// Driven by updating the row directly rather than through the approval endpoints,
	// because what is under test is how THIS code reads each state — not how the app
	// transitions between them, which has its own tests.
	for _, c := range []struct {
		name    string
		status  string
		want    mcpBusiness.WriteOutcome
		because string
	}{
		{
			name: "mid-execution blocks a retry", status: pendingModels.StatusExecuting,
			want: mcpBusiness.WriteAlreadyApplied,
			because: "a call that is running must not be started again by a retry arriving a " +
				"moment later. Reading 'in progress' as 'not yet done' is exactly how a " +
				"duplicate write happens, and it is the tempting reading because the work " +
				"genuinely has not finished.",
		},
		{
			name: "executed reports success without re-running", status: pendingModels.StatusExecuted,
			want: mcpBusiness.WriteAlreadyApplied,
			because: "the caller's intent has been applied, just not by this request — so " +
				"success is honest and re-executing is the duplicate the key exists to stop",
		},
		{
			name: "a denial is final", status: pendingModels.StatusRejected,
			want:    mcpBusiness.WriteRefused,
			because: "a human said no; retrying must not quietly ask someone else",
		},
		{
			name: "an expired card can be asked again", status: pendingModels.StatusExpired,
			want: mcpBusiness.WriteNeedsApproval,
			because: "the approval window closed without a decision, so asking again is " +
				"correct rather than refusing forever",
		},
		{
			name: "a failed write frees the key", status: pendingModels.StatusFailed,
			want: mcpBusiness.WriteNeedsApproval,
			because: "a write that errored did not take effect, so an agent must be able to " +
				"recover from a transient fault rather than being blocked with the failure " +
				"looking like success",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := env.PG.Exec(
				`UPDATE ai_pending_actions SET status = $1 WHERE idempotency_key = $2`,
				c.status, key,
			); err != nil {
				t.Fatalf("set status %s: %v", c.status, err)
			}

			plan := mcpBusiness.PlanWrite(ctx, spec, call, "test-client", mcpBusiness.CheckExistingWrite)
			if plan.Outcome != c.want {
				t.Fatalf("status %q produced outcome %q, want %q.\n\n%s",
					c.status, plan.Outcome, c.want, c.because)
			}
		})
	}
}

// seedUser inserts the minimum users row the pending-action foreign key needs and
// returns its id.
func seedUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	id := uuid.NewString()
	// Only id and email_id: everything else on users is nullable or defaulted, and
	// inserting more would couple this test to columns later migrations add.
	if _, err := db.Exec(
		`INSERT INTO users (id, email_id) VALUES ($1, $2)`,
		id, id[:8]+"@example.test",
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}
