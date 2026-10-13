//go:build integration
// +build integration

package integration_test

// A directory app's commands reach the "/" menu only once the app has an
// address to send them to, against a real Postgres.
//
// Installing Jira from the directory put /jira in every member's "/" menu at
// once, and the command answered "has no handler configured" until an admin
// found the app editor's address field, which the directory never asked for
// (SA-BUG-8). The menu now leaves it out until there is an address, and the
// directory says the app needs setup.
//
// Run: go test -tags=integration ./tests/integration/ -run TestADirectoryAppsCommandsWaitForItsAddress -v

import (
	"context"
	"testing"

	"github.com/google/uuid"

	commandBusiness "github.com/akashc777/OneCamp/business/Command"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestADirectoryAppsCommandsWaitForItsAddress(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	admin := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'admin@example.test', 'admin', 'Admin', NOW(), NOW())`, admin); err != nil {
		t.Fatal(err)
	}
	var member userModels.UserInfo
	member.UserPostgresInfo.Id = uuid.New()
	member.UserDgraphInfo = dgraphStruct.DgraphUser{Uuid: member.UserPostgresInfo.Id.String()}

	app, err := commandBusiness.InstallTemplate(ctx, "jira", admin)
	if err != nil || app == nil {
		t.Fatalf("installing Jira from the directory: %v", err)
	}

	inMenu := func(t *testing.T) bool {
		t.Helper()
		catalog, err := commandBusiness.GetCatalog(ctx, member)
		if err != nil {
			t.Fatalf("reading the / menu: %v", err)
		}
		for _, c := range catalog.Commands {
			if c.Command == "jira" {
				return true
			}
		}
		return false
	}
	needsSetup := func(t *testing.T) bool {
		t.Helper()
		items, err := commandBusiness.ListMarketplace(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Slug == "jira" {
				return it.NeedsSetup
			}
		}
		t.Fatal("Jira isn't in the directory")
		return false
	}

	if inMenu(t) {
		t.Error("/jira is in the / menu with nowhere to send it")
	}
	if !needsSetup(t) {
		t.Error("Jira counts as set up with no address for its commands")
	}

	appID := uuid.MustParse(app.ID)
	if err := slashModel.UpdateApp(ctx, appID, map[string]interface{}{"handler_url": "https://hooks.example.test/jira"}); err != nil {
		t.Fatalf("giving Jira its address: %v", err)
	}
	if !inMenu(t) {
		t.Error("/jira stays out of the / menu after the app got its address")
	}
}
