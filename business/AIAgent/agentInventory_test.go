package business

import (
	"context"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	tokenModel "github.com/akashc777/OneCamp/models/postgres/ApiToken"
	"github.com/google/uuid"
)

func TestBuildInventory(t *testing.T) {
	priya, sam := uuid.New(), uuid.New()
	quiet := &model.AiAgent{Id: uuid.New(), Name: "Quiet", IsActive: true, CreatedBy: priya,
		EnabledTools: `["send_message"]`, Scope: `{}`, TriggerType: "mention", Autonomy: "act"}
	refused := &model.AiAgent{Id: uuid.New(), Name: "Release Captain", IsActive: true, CreatedBy: priya,
		EnabledTools: `["create_task","send_message"]`, Scope: `{"channel_ids":["c1","c2"]}`,
		AGUIEndpoint: "https://brain.example.com/a2a", RemoteProtocol: model.RemoteA2A}
	paused := &model.AiAgent{Id: uuid.New(), Name: "Paused", IsActive: false, CreatedBy: sam,
		EnabledTools: `[]`, Scope: `{}`}

	bound := &tokenModel.ApiToken{Id: uuid.New(), Name: "captain key", Scopes: `["tasks:write"]`,
		CreatedBy: priya, AgentId: &refused.Id}
	loose := &tokenModel.ApiToken{Id: uuid.New(), Name: "ci script", Scopes: ``, CreatedBy: sam}

	last := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	inv := buildInventory(
		[]*model.AiAgent{quiet, refused, paused},
		map[string]*model.AgentHealth{quiet.Id.String(): {Last7dRuns: 9}, refused.Id.String(): {Last7dRuns: 2}},
		map[string]*model.AgentActivity{refused.Id.String(): {Actions: 3, Refusals: 1, LastRefusalAt: &last}},
		map[string]*model.AgentActivity{loose.Id.String(): {Refusals: 4}},
		[]*tokenModel.ApiToken{bound, loose},
		map[uuid.UUID]string{priya: "Priya N"},
	)

	if inv.WindowDays != 7 {
		t.Fatalf("window = %d, want 7", inv.WindowDays)
	}
	names := []string{inv.Agents[0].Name, inv.Agents[1].Name, inv.Agents[2].Name}
	if names[0] != "Release Captain" || names[1] != "Quiet" || names[2] != "Paused" {
		t.Fatalf("order = %v: refused first, then active, then paused", names)
	}
	rc := inv.Agents[0]
	if rc.Sponsor != "Priya N" || rc.Channels != 2 || rc.Tools != 0 || rc.Credentials != 1 ||
		rc.Actions7d != 3 || rc.Refusals7d != 1 || rc.Runs7d != 2 || rc.LastRefusalAt == nil {
		t.Fatalf("release captain row = %+v", rc)
	}
	if rc.Brain != "a2a:brain.example.com" {
		t.Fatalf("brain = %q", rc.Brain)
	}
	if inv.Agents[1].Tools != 1 {
		t.Fatalf("a local agent reaches its enabled tools, got %d", inv.Agents[1].Tools)
	}
	if inv.Agents[1].Brain != "workspace" {
		t.Fatalf("a local agent's brain = %q", inv.Agents[1].Brain)
	}
	if inv.Agents[2].Sponsor != "" {
		t.Fatalf("an unresolved sponsor must stay empty, got %q", inv.Agents[2].Sponsor)
	}

	c0, c1 := inv.Credentials[0], inv.Credentials[1]
	if c0.AgentName != "Release Captain" || len(c0.Scopes) != 1 {
		t.Fatalf("bound credential = %+v", c0)
	}
	if c1.AgentID != nil || c1.Refusals7d != 4 || c1.Scopes == nil || len(c1.Scopes) != 0 {
		t.Fatalf("loose credential = %+v", c1)
	}
}

func TestAgentInventoryScopesMembersToTheirOwn(t *testing.T) {
	me, other := uuid.New(), uuid.New()
	defer func(a, h, act, tok, n any) {
		inventoryAgentsFn = a.(func(context.Context, Actor) ([]*model.AiAgent, error))
		inventoryHealthFn = h.(func(context.Context, Actor) (map[string]*model.AgentHealth, error))
		inventoryActivityFn = act.(func(context.Context, time.Time) (map[string]*model.AgentActivity, map[string]*model.AgentActivity, error))
		inventoryTokensFn = tok.(func(context.Context) ([]*tokenModel.ApiToken, error))
		inventoryNamesFn = n.(func(context.Context, []uuid.UUID) (map[uuid.UUID]string, error))
	}(inventoryAgentsFn, inventoryHealthFn, inventoryActivityFn, inventoryTokensFn, inventoryNamesFn)

	inventoryAgentsFn = func(context.Context, Actor) ([]*model.AiAgent, error) { return nil, nil }
	inventoryHealthFn = func(context.Context, Actor) (map[string]*model.AgentHealth, error) { return nil, nil }
	inventoryActivityFn = func(context.Context, time.Time) (map[string]*model.AgentActivity, map[string]*model.AgentActivity, error) {
		return nil, nil, nil
	}
	inventoryTokensFn = func(context.Context) ([]*tokenModel.ApiToken, error) {
		return []*tokenModel.ApiToken{
			{Id: uuid.New(), CreatedBy: me, Scopes: `[]`},
			{Id: uuid.New(), CreatedBy: other, Scopes: `[]`},
		}, nil
	}
	var looked []uuid.UUID
	inventoryNamesFn = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
		looked = ids
		return map[uuid.UUID]string{}, nil
	}

	inv, err := AgentInventory(context.Background(), Actor{UserID: me})
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Credentials) != 1 || inv.Credentials[0].SponsorID != me {
		t.Fatalf("a member saw %d credentials, want only their own", len(inv.Credentials))
	}
	if len(looked) != 1 || looked[0] != me {
		t.Fatalf("names looked up for %v", looked)
	}

	inv, _ = AgentInventory(context.Background(), Actor{UserID: me, IsAdmin: true})
	if len(inv.Credentials) != 2 {
		t.Fatalf("an admin saw %d credentials, want 2", len(inv.Credentials))
	}
}

func TestRevokeCredentialIsAdminOnly(t *testing.T) {
	if _, err := RevokeCredential(context.Background(), uuid.New(), Actor{UserID: uuid.New()}); !IsForbidden(err) {
		t.Fatalf("a member revoking = %v, want forbidden", err)
	}
}
