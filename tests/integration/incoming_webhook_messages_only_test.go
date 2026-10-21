//go:build integration
// +build integration

package integration_test

// An incoming webhook only posts messages (platform P-API part 3; the one
// command registry is business/Command). Against a real Postgres with every
// migration:
//   - the webhook's own command registry was dead: migration 46 gave every
//     webhook commands = '[]', which no code has ever written since (no
//     INSERT or UPDATE of webhooks names the column), and '[]' parsed to no
//     commands, so even /help and /status answered "Unknown command";
//     webhook_commands, made beside it, was never written either;
//   - a payload that names a command is told where slash commands are now.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnIncomingWebhookOnlyPostsMessages -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	webhookController "github.com/akashc777/OneCamp/controllers/Webhook"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnIncomingWebhookOnlyPostsMessages(t *testing.T) {
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
	hook, err := webhookBusiness.CreateWebhook(ctx, webhookBusiness.WebhookCreateInput{Name: "Alerts", Type: "incoming", BotName: "Alerts"}, owner)
	if err != nil {
		t.Fatal(err)
	}

	// What the old registry read: '[]' on every webhook, and no rows beside it.
	var notEmpty, registered int
	if err := env.PG.QueryRow(`SELECT count(*) FROM webhooks WHERE commands IS DISTINCT FROM '[]'::jsonb`).Scan(&notEmpty); err != nil {
		t.Fatal(err)
	}
	if err := env.PG.QueryRow(`SELECT count(*) FROM webhook_commands`).Scan(&registered); err != nil {
		t.Fatal(err)
	}
	if notEmpty != 0 || registered != 0 {
		t.Fatalf("a webhook holds commands: %d with a list, %d registered rows", notEmpty, registered)
	}

	send := func(body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/webhook/incoming/"+hook.Token, strings.NewReader(body))
		rc := chi.NewRouteContext()
		rc.URLParams.Add("token", hook.Token)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rc))
		rec := httptest.NewRecorder()
		webhookController.HandleIncomingWebhook(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := send(`{"command": "/status", "text": "status please"}`)
	if code != http.StatusBadRequest || out["error"] != webhookBusiness.SlashCommandsMoved {
		t.Fatalf("a command: %d %v", code, out)
	}
}
