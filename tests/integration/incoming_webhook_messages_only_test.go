//go:build integration
// +build integration

package integration_test

// An incoming webhook only posts messages, in a channel, as the automation
// bot. Against a real Postgres with every migration:
//   - the webhook's own command registry was dead (platform P-API part 3; the
//     one command registry is business/Command): migration 46 gave every
//     webhook commands = '[]', which no code has ever written since (no
//     INSERT or UPDATE of webhooks names the column), and '[]' parsed to no
//     commands, so even /help and /status answered "Unknown command";
//     webhook_commands, made beside it, was never written either. A payload
//     that names a command is told where slash commands are now;
//   - a payload that named a person (dm_id) or a group chat (group_chat_id)
//     was written as the admin who made the webhook (A11), so anyone holding
//     its URL could message any member from that admin's account. It is
//     refused, and nothing is written; a channel still gets the bot's post
//     (with Dgraph, which the channel's membership is read from).
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAnIncomingWebhookOnlyPostsMessages|TestAnIncomingWebhookNeverWritesAsItsCreator' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	webhookController "github.com/akashc777/OneCamp/controllers/Webhook"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

// sendToIncomingWebhook posts body to the incoming webhook whose path token is
// token, as its sender would, and reads the answer.
func sendToIncomingWebhook(t *testing.T, token, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/incoming/"+token, strings.NewReader(body))
	rc := chi.NewRouteContext()
	rc.URLParams.Add("token", token)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rc))
	rec := httptest.NewRecorder()
	webhookController.HandleIncomingWebhook(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

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
	// Only the token's hash is kept: a copy of the table posts as nobody.
	if n := webhooksHolding(t, env.PG, hook.Token); hook.Token == "" || n != 0 {
		t.Fatalf("the token %q is kept in the clear in %d webhooks", hook.Token, n)
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

	code, out := sendToIncomingWebhook(t, hook.Token, `{"command": "/status", "text": "status please"}`)
	if code != http.StatusBadRequest || out["error"] != webhookBusiness.SlashCommandsMoved {
		t.Fatalf("a command: %d %v", code, out)
	}
}

func TestAnIncomingWebhookNeverWritesAsItsCreator(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// The admin who made the webhook, in its channel and in a group chat with
	// a member.
	admin, member, channel := uuid.New(), uuid.New(), uuid.New()
	group := uuid.NewString()
	for _, p := range []struct {
		id   uuid.UUID
		name string
	}{{admin, "ada"}, {member, "sam"}} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			p.id, p.id.String()+"@example.test", p.name, p.name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'alerts', false)`, channel); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:admin", "user_uuid": admin.String(), "user_name": "ada", "user_full_name": "Ada",
			"user_channels": []map[string]any{{"uid": "_:channel"}},
			"user_dms":      []map[string]any{{"uid": "_:group"}}},
		{"uid": "_:member", "user_uuid": member.String(), "user_name": "sam", "user_full_name": "Sam",
			"user_dms": []map[string]any{{"uid": "_:group"}}},
		{"uid": "_:channel", "ch_uuid": channel.String(), "ch_name": "alerts", "ch_handle": "alerts", "ch_private": false,
			"ch_members": []map[string]any{{"uid": "_:admin"}}},
		{"uid": "_:group", "dm_grouping_id": group,
			"dm_participants": []map[string]any{{"uid": "_:admin"}, {"uid": "_:member"}}},
	})
	hook, err := webhookBusiness.CreateWebhook(ctx, webhookBusiness.WebhookCreateInput{Name: "Billing", Type: "incoming", BotName: "Billing"}, admin)
	if err != nil {
		t.Fatal(err)
	}

	// What a leaked URL would send: a lure, to people.
	lure := `"text": "Your invoice is overdue: pay at https://example.test/pay"`
	for _, c := range []struct{ name, body string }{
		{"a DM", `{` + lure + `, "dm_id": "` + member.String() + `"}`},
		{"a group chat", `{` + lure + `, "group_chat_id": "` + group + `"}`},
		{"a DM beside a channel", `{` + lure + `, "channel_id": "` + channel.String() + `", "dm_id": "` + member.String() + `"}`},
	} {
		if code, out := sendToIncomingWebhook(t, hook.Token, c.body); code != http.StatusBadRequest || out["error"] != webhookBusiness.DirectMessagesMoved {
			t.Errorf("%s: %d %v, want 400 %q", c.name, code, out, webhookBusiness.DirectMessagesMoved)
		}
	}
	var chats, posts int
	if err := env.PG.QueryRow(`SELECT count(*) FROM chats WHERE created_by = $1`, admin).Scan(&chats); err != nil {
		t.Fatal(err)
	}
	if err := env.PG.QueryRow(`SELECT count(*) FROM posts WHERE post_channel = $1`, channel).Scan(&posts); err != nil {
		t.Fatal(err)
	}
	if chats != 0 || posts != 0 {
		t.Fatalf("refused payloads wrote %d chats as the webhook's creator, and %d posts", chats, posts)
	}

	// The channel still gets its message, written by the automation bot.
	code, out := sendToIncomingWebhook(t, hook.Token, `{"text": "Deploy 41 is live", "channel_id": "`+channel.String()+`"}`)
	if code != http.StatusOK {
		t.Fatalf("a channel message: %d %v", code, out)
	}
	var by uuid.UUID
	if err := env.PG.QueryRow(`SELECT created_by FROM posts WHERE id = $1`, out["post_id"]).Scan(&by); err != nil {
		t.Fatal(err)
	}
	if bot := userBusiness.GetAutomationBot(ctx); bot == nil || by != bot.UserID {
		t.Fatalf("the channel's post was written by %s, not the automation bot (the creator is %s)", by, admin)
	}
}
