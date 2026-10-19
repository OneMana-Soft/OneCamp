//go:build integration
// +build integration

package integration_test

// An API token acts at most as a member over MCP, against real Postgres, Dgraph
// and Redis, through AuthorizeToolCall: the seam every MCP call passes.
//
// A token carries none of its owner's admin powers (the owner's decision of
// 16 Oct 2026; accessBusiness.TokenSubjectOf states it for docs). MCP's reach
// gave them anyway in three places: a table's and a data source's rule took the
// owner's workspace-admin flag, which lets an admin see and change every table
// and query every source, and a team's write rule let a team admin's token
// create in the team. What an admin may do, they do in the app, as themselves.
// A person asking an agent in the app is not a token, and keeps their team role.
//
// Run: go test -tags=integration ./tests/integration/ -run TestATokenActsAsAMemberOverMCP -v

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	mcpBusiness "github.com/akashc777/OneCamp/business/MCPServer"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dataSourceModel "github.com/akashc777/OneCamp/models/postgres/DataSource"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestATokenActsAsAMemberOverMCP(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	person := func(email, name string) userModels.UserInfo {
		t.Helper()
		id, _, err := userBusiness.CreateUserWithMethod(ctx, email, name, nil, userModels.AuthMethodEmail, false)
		if err != nil {
			t.Fatalf("make %s: %v", name, err)
		}
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", name, err)
		}
		node, err := userDomain.GetDgraphUserInfoByUUID(ctx, id.String())
		if err != nil || node == nil {
			t.Fatalf("load %s's node: %v", name, err)
		}
		return userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *node}
	}
	// Ana is a workspace admin, and an admin of the Platform team. Bo is a member.
	ana := person("ana@example.test", "Ana")
	if err := userBusiness.CreateAdminUser(ctx, "ana@example.test"); err != nil {
		t.Fatal(err)
	}
	bo := person("bo@example.test", "Bo")
	anaID := ana.UserPostgresInfo.Id.String()

	// The surface is on with every tool group, so nothing but authority decides.
	if err := mcpBusiness.SetAdmission(ctx, true, "*"); err != nil {
		t.Fatalf("turn MCP on: %v", err)
	}
	if err := mcpBusiness.RegisterBridgedTools(); err != nil && !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("register the MCP tools: %v", err)
	}

	// Bo's tables: one only he sees, one the whole workspace sees.
	boActor := dataTableBusiness.Actor{UserID: bo.UserPostgresInfo.Id}
	salaries, err := dataTableBusiness.CreateTable(ctx, dataTableBusiness.TableInput{Name: "Salaries", Visibility: "private"}, boActor)
	if err != nil {
		t.Fatalf("make Bo's private table: %v", err)
	}
	roadmap, err := dataTableBusiness.CreateTable(ctx, dataTableBusiness.TableInput{Name: "Roadmap", Visibility: "workspace"}, boActor)
	if err != nil {
		t.Fatalf("make Bo's workspace table: %v", err)
	}
	// Bo's data source, which only he may query.
	finance, err := dataSourceModel.Create(ctx, &dataSourceModel.DataSource{
		Name: "Finance", Engine: dataSourceModel.EnginePostgres, Host: "warehouse.example.test", Port: 5432,
		Database: "finance", Username: "reader", SSLMode: dataSourceModel.SSLRequire,
		Visibility: dataSourceModel.VisibilityPrivate, Enabled: true, CreatedBy: bo.UserPostgresInfo.Id,
	})
	if err != nil {
		t.Fatalf("make Bo's data source: %v", err)
	}
	// The Platform team, with Ana its admin.
	team := uuid.NewString()
	dg.Mutate(t, []map[string]any{{
		"uid": "_:team", "dgraph.type": "Team", "team_uuid": team, "team_name": "Platform",
		"team_deleted_at": "0001-01-01T00:00:00Z",
		"team_admins":     []map[string]any{{"uid": ana.UserDgraphInfo.Uid}},
		"team_members":    []map[string]any{{"uid": ana.UserDgraphInfo.Uid}},
	}})

	// Ana's token, with every scope: what it may do is her reach, as a member.
	made, err := apiTokenBusiness.CreateToken(ctx, "Ana's script", apiTokenBusiness.AllScopes, 30, ana.UserPostgresInfo.Id, nil, true)
	if err != nil {
		t.Fatalf("make Ana's token: %v", err)
	}
	call := func(tool string, args map[string]any) mcpBusiness.Decision {
		return mcpBusiness.AuthorizeToolCall(ctx, mcpBusiness.ToolCallRequest{TokenPlaintext: made.Plaintext, ToolName: tool, Args: args})
	}
	refused := func(t *testing.T, what string, d mcpBusiness.Decision) {
		t.Helper()
		if d.Allow {
			t.Errorf("%s: allowed (%s), want refused: a token acts at most as a member", what, d.Reason)
		}
	}

	t.Run("a workspace admin's token is refused a private table only the admin override reaches", func(t *testing.T) {
		refused(t, "read_table on Bo's private table", call("read_table", map[string]any{"table_uuid": salaries.Id.String()}))
		refused(t, "query_table on Bo's private table", call("query_table", map[string]any{"table_uuid": salaries.Id.String()}))
	})
	t.Run("a workspace admin's token is refused changing a table it may only see", func(t *testing.T) {
		refused(t, "update_table_row on Bo's workspace table", call("update_table_row",
			map[string]any{"table_uuid": roadmap.Id.String(), "row_uuid": uuid.NewString(), "values": map[string]any{}}))
	})
	t.Run("a workspace admin's token still reads a table open to the workspace, as any member does", func(t *testing.T) {
		if d := call("read_table", map[string]any{"table_uuid": roadmap.Id.String()}); !d.Allow {
			t.Errorf("read_table on Bo's workspace table: refused (%s), want allowed", d.Reason)
		}
	})
	t.Run("a workspace admin's token is refused a data source only the admin override reaches", func(t *testing.T) {
		refused(t, "read_data_source on Bo's private source", call("read_data_source", map[string]any{"data_source_uuid": finance.String()}))
		refused(t, "query_data_source on Bo's private source", call("query_data_source",
			map[string]any{"data_source_uuid": finance.String(), "sql": "select 1"}))
	})
	t.Run("a team admin's token is refused creating in the team", func(t *testing.T) {
		d := call("create_project", map[string]any{"team_uuid": team, "name": "Launch"})
		refused(t, "create_project in Ana's team", d)
		if !d.Allow && !strings.Contains(d.Reason, "token") {
			t.Errorf("the refusal says %q, want it to say a token acts as a member", d.Reason)
		}
	})
	t.Run("asking an agent in the app, the team admin keeps her role", func(t *testing.T) {
		ref := mcpBusiness.ResourceRef{Kind: mcpBusiness.ResourceTeam, ID: team, Access: mcpBusiness.AccessWrite}
		if d := mcpBusiness.PrincipalCanReach(ctx, anaID, ref); !d.Allow {
			t.Errorf("Ana asking an agent to create in her team: refused (%s), want allowed", d.Reason)
		}
		// Bo is no admin of it, in the app or anywhere.
		if d := mcpBusiness.PrincipalCanReach(ctx, bo.UserPostgresInfo.Id.String(), ref); d.Allow {
			t.Errorf("Bo asking an agent to create in a team he isn't in: allowed (%s)", d.Reason)
		}
	})
}
