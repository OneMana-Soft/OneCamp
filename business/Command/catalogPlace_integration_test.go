//go:build integration

package business

// The "/" menu through a Giphy app's whole life, against Postgres and Redis:
// a member is offered /giphy only once it works, an admin is offered to set
// it up until then (an admin's API token acts as a member), and turning it off
// or removing it takes it from everyone. A command this build can't run is in
// nobody's menu. Each step reads through the menu's cache, as the composer does.
// Run: PKGS=./business/Command/ integ_rel.sh <worktree> <log> TestTheMenu

import (
	"context"
	"testing"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestTheMenuOffersGiphyOnlyWhereItWorks(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	SeedBuiltinCommands(ctx)
	// /remind is in every edition's menu; /ask only where this build registers
	// the AI package (without-ai seeds it with no handler, like unwired below).
	wantBuiltins := 1
	if helpers.FeatureRegistered(helpers.FeatureNameAI) {
		wantBuiltins = 2
	}
	// A built-in seeded where this build wires no handler for it, as without-ai
	// seeds /ask: it would only answer that it's unavailable, so it's in no menu.
	unwired := "unwired"
	if err := slashModel.UpsertCommand(ctx, &slashModel.SlashCommand{Command: unwired, Description: "Seeded with no handler",
		ExecMode: slashModel.ExecInline, ScopeType: "org", ResponseType: "ephemeral", IsBuiltin: true, IsEnabled: true}); err != nil {
		t.Fatal(err)
	}

	adminID := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, 'ada', 'Ada', NOW(), NOW())`, adminID, adminID.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	person := func(admin bool) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = uuid.New()
		u.UserPostgresInfo.IsAdmin = admin
		u.UserDgraphInfo.Uuid = u.UserPostgresInfo.Id.String()
		return u
	}
	member, admin := person(false), person(true)
	token := helpers.WithActsThroughToken(ctx)
	giphy := func(ctx context.Context, u userModels.UserInfo) *commandAdapter.CatalogCommand {
		t.Helper()
		menu, err := GetCatalog(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		var found *commandAdapter.CatalogCommand
		builtins := 0
		for i, c := range menu.Commands {
			if c.Command == "giphy" {
				found = &menu.Commands[i]
			}
			if c.Command == "remind" || c.Command == "ask" {
				builtins++
			}
			if c.Command == unwired {
				t.Fatalf("the menu offers /%s, which this build has no handler for", unwired)
			}
		}
		if builtins != wantBuiltins {
			t.Fatalf("the menu lost /remind or /ask: %+v", menu.Commands)
		}
		return found
	}
	// want: "" not offered, "run" offered as it is, "setup" offered to set up.
	expect := func(when string, wantMember, wantAdmin string) {
		t.Helper()
		for _, who := range []struct {
			name string
			ctx  context.Context
			u    userModels.UserInfo
			want string
		}{{"member", ctx, member, wantMember}, {"admin", ctx, admin, wantAdmin}, {"admin's API token", token, admin, wantMember}} {
			got := ""
			if c := giphy(who.ctx, who.u); c != nil && c.Setup == nil {
				got = "run"
			} else if c != nil {
				got = "setup"
				if c.Setup.Slug != giphyAppSlug || c.Setup.Name != "Giphy" {
					t.Fatalf("%s, %s: set up %+v; want Giphy's slug and name", when, who.name, c.Setup)
				}
			}
			if got != who.want {
				t.Fatalf("%s: the %s is offered /giphy as %q; want %q", when, who.name, got, who.want)
			}
		}
	}

	expect("never installed", "", "setup")

	app, err := InstallTemplate(ctx, giphyAppSlug, adminID)
	if err != nil {
		t.Fatal(err)
	}
	appID, err := uuid.Parse(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	expect("installed with no key", "", "setup")

	if _, err := UpdateApp(ctx, appID, commandAdapter.UpdateAppRequest{Secrets: map[string]string{"api_key": "key-1"}}); err != nil {
		t.Fatal(err)
	}
	expect("with its key", "run", "run")

	if err := SetAppEnabled(ctx, appID, false); err != nil {
		t.Fatal(err)
	}
	expect("turned off", "", "")

	if err := SetAppEnabled(ctx, appID, true); err != nil {
		t.Fatal(err)
	}
	if err := DeleteApp(ctx, appID); err != nil {
		t.Fatal(err)
	}
	expect("removed", "", "")

	// Installed again, the new app holds no key: removed rows never stand in for it.
	if _, err := InstallTemplate(ctx, giphyAppSlug, adminID); err != nil {
		t.Fatal(err)
	}
	expect("installed again", "", "setup")
}
