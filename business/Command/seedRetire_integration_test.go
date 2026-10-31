//go:build integration

package business

// The edition without AI kept /ask in every "/" menu (23 Oct 2026), with no
// handler for it in the build: its seed listed /ask, and seeding only ever
// added, so a row an earlier seed put there stayed too. Against Postgres:
// after seeding, every built-in the catalog offers has its handler in this
// build; one the build doesn't carry is taken out, and nothing else is (an
// app's own built-in stays with its app); taking one out moves the catalog's
// version, so menus cached before it are read again; a build that carries it
// again offers it again.
// Run: go test -tags=integration ./business/Command/ -run TestTheSeededCatalogOffersOnlyWhatTheBuildAnswers -v

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestTheSeededCatalogOffersOnlyWhatTheBuildAnswers(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	ask := builtinSpec{"ask", "Ask OneCamp AI a quick question", "<your question>", slashModel.ExecInline, "ephemeral"}
	upsert := func(command string, builtin bool) {
		t.Helper()
		if err := slashModel.UpsertCommand(ctx, &slashModel.SlashCommand{
			Command: command, Description: command, ExecMode: slashModel.ExecInline,
			ScopeType: "org", ResponseType: "ephemeral", IsBuiltin: builtin, IsEnabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// The catalog every member's "/" reads.
	offered := func() map[string]*slashModel.SlashCommand {
		t.Helper()
		cmds, err := slashModel.ListCommandsForScopes(ctx, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*slashModel.SlashCommand{}
		for _, c := range cmds {
			out[c.Command] = c
		}
		return out
	}

	// An earlier build seeded /ask and a built-in since dropped; a workspace has a command of its own.
	upsert(ask.command, true)
	upsert("lunch", true)
	upsert("standup", false)
	app := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO apps (id, slug, name) VALUES ($1, 'deployer', 'Deployer')`, app); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO slash_commands (command, app_id, description, is_builtin, is_enabled)
		VALUES ('deploy', $1, 'deploy', true, true)`, app); err != nil {
		t.Fatal(err)
	}

	before := catalogVersion(ctx)
	SeedBuiltinCommands(ctx)
	if catalogVersion(ctx) == before {
		t.Error("taking out /lunch left the catalog's version as it was, so a menu cached before it still offers it")
	}
	var appsLive bool
	if err := env.PG.QueryRow(`SELECT deleted_at IS NULL FROM slash_commands WHERE command = 'deploy'`).Scan(&appsLive); err != nil || !appsLive {
		t.Errorf("the app's own built-in /deploy was taken out with the build's (live %v, %v)", appsLive, err)
	}
	now := offered()
	for name, c := range now {
		if _, answered := commandRegistry[name]; c.IsBuiltin && c.AppId == nil && !answered {
			t.Errorf("the catalog offers the built-in /%s, and this build has no handler for it", name)
		}
	}
	for _, b := range builtinCatalog {
		if now[b.command] == nil {
			t.Errorf("/%s, which this build seeds, isn't offered", b.command)
		}
	}
	if now["standup"] == nil {
		t.Error("the workspace's own /standup was taken out with the built-ins")
	}

	// A build that carries /ask (the AI line) offers it again.
	if _, carried := commandRegistry[ask.command]; !carried {
		kept := builtinCatalog
		builtinCatalog = append(append([]builtinSpec{}, builtinCatalog...), ask)
		t.Cleanup(func() { builtinCatalog = kept })
		SeedBuiltinCommands(ctx)
		if offered()[ask.command] == nil {
			t.Error("a build that seeds /ask again doesn't offer it")
		}
	}
}
