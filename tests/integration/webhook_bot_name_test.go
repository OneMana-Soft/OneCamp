//go:build integration
// +build integration

package integration_test

// An incoming webhook's messages always carry a name, against a real
// Postgres: a name of only spaces is the default, as an empty one is, at
// create and at update. The name is the label its messages are posted under,
// and a message with no label is read as a model's Markdown
// (business/BotPost badgeHTML); a webhook's text is whatever its URL's holder
// sends, so it must never reach that path.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAWebhookAlwaysHasAName -v

import (
	"context"
	"testing"

	"github.com/google/uuid"

	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAWebhookAlwaysHasAName(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	owner := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'ci', 'CI', NOW(), NOW())`,
		owner, owner.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}

	blank, err := webhookBusiness.CreateWebhook(ctx, webhookBusiness.WebhookCreateInput{Name: "Alerts", Type: "incoming", BotName: "   "}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if blank.BotName != "Webhook Bot" {
		t.Fatalf("a webhook named only spaces is named %q, want the default", blank.BotName)
	}
	padded, err := webhookBusiness.CreateWebhook(ctx, webhookBusiness.WebhookCreateInput{Name: "Deploys", Type: "incoming", BotName: "  Deploys  "}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if padded.BotName != "Deploys" {
		t.Fatalf("a padded name is stored as %q", padded.BotName)
	}
	kept, _, err := webhookBusiness.UpdateWebhook(ctx, padded.Id, webhookBusiness.WebhookUpdateInput{Name: "Deploys", IsActive: true, BotName: " \t "}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if kept.BotName != "Deploys" {
		t.Fatalf("an update naming only spaces left the name %q, want it kept", kept.BotName)
	}
}
