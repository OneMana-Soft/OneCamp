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

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	commandBusiness "github.com/akashc777/OneCamp/business/Command"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
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

	// Set up the way an admin does it, through the admin's own path, which moves
	// the catalog's version on. Jira is set up once its address and its OAuth
	// client are both in: a member's "/" menu leaves out an app that still needs
	// setting up (commandPlace), so the address alone isn't enough.
	appID := uuid.MustParse(app.ID)
	// A public address literal, as command_responses_test uses: the admin's
	// path refuses every special range (TEST-NET-3 among them) and would look
	// a name up, so a literal public address is checked without the network.
	// Nothing dials it, nor the OAuth addresses: the menu only reads them.
	address := "https://93.184.216.34/jira"
	if _, err := commandBusiness.UpdateApp(ctx, appID, commandAdapter.UpdateAppRequest{
		HandlerURL: &address,
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			ClientID:     "jira-client",
			ClientSecret: "jira-client-secret",
			AuthURL:      "https://auth.atlassian.com/authorize",
			TokenURL:     "https://auth.atlassian.com/oauth/token",
			Scopes:       []string{"read:jira-work", "write:jira-work"},
		},
	}); err != nil {
		t.Fatalf("setting Jira up: %v", err)
	}
	if needsSetup(t) {
		t.Error("Jira still needs setting up with its address and OAuth client in")
	}
	if !inMenu(t) {
		t.Error("/jira stays out of the / menu once the app was set up")
	}
}
