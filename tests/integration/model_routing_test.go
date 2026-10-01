//go:build integration
// +build integration

package integration_test

// Routing background work to a model must stay inside the allowlist: the
// allowlist is the admin's statement of which models may see workspace
// content, and routing must not become a way around it. Against real
// Postgres: an unknown purpose and an unlisted or disabled model are refused,
// a listed one is saved and read back, and an empty model means "default".

import (
	"context"
	"errors"
	"testing"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestModelRoutingStaysInsideTheAllowlist(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	p, err := aiModels.CreateCustomProvider(ctx, "routing test", "http://127.0.0.1:9/v1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := aiModels.CreateAuthorizedModel(ctx, p.ID, "fast-model", "Fast")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		routes map[string]aiModels.RouteTarget
		want   error
	}{
		{"unknown purpose", map[string]aiModels.RouteTarget{"chat": {ProviderID: p.ID, Model: "fast-model"}}, nil},
		{"unlisted model", map[string]aiModels.RouteTarget{ai.PurposeSummaries: {ProviderID: p.ID, Model: "not-listed"}}, aiBusiness.ErrRouteNotAllowed},
		{"unknown provider", map[string]aiModels.RouteTarget{ai.PurposeSummaries: {ProviderID: uuid.New(), Model: "fast-model"}}, aiBusiness.ErrRouteNotAllowed},
	}
	for _, c := range cases {
		err := aiBusiness.SetModelRouting(ctx, c.routes)
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: got %v, want a refusal", c.name, err)
		}
	}
	if got, _ := aiModels.GetModelRouting(ctx); len(got) != 0 {
		t.Fatalf("a refused route was saved: %+v", got)
	}

	// A listed model is accepted. The reload that follows may fail here (no AI
	// is configured in this database); what matters is what was stored.
	_ = aiBusiness.SetModelRouting(ctx, map[string]aiModels.RouteTarget{
		ai.PurposeSummaries: {ProviderID: p.ID, Model: listed.Model},
		ai.PurposeMemory:    {ProviderID: p.ID, Model: ""},
	})
	got, err := aiModels.GetModelRouting(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[ai.PurposeSummaries].Model != "fast-model" {
		t.Fatalf("routing stored %+v, want summaries -> fast-model and nothing else", got)
	}

	// A model switched off on the allowlist can no longer be routed to.
	if _, err := env.PG.Exec(`UPDATE ai_authorized_models SET enabled = false WHERE id = $1`, listed.ID); err != nil {
		t.Fatal(err)
	}
	if err := aiBusiness.SetModelRouting(ctx, map[string]aiModels.RouteTarget{
		ai.PurposeMeetings: {ProviderID: p.ID, Model: listed.Model},
	}); !errors.Is(err, aiBusiness.ErrRouteNotAllowed) {
		t.Fatalf("routed to a disabled model: %v", err)
	}
}
