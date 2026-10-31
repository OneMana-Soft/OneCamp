//go:build integration
// +build integration

package integration_test

// An outgoing webhook is sent only the kinds of event it names (V-112), against
// real Postgres 12 and Dgraph, through the controllers. One with none ticked was
// sent nothing while the dialog said it got every event, and a change that
// named no kinds emptied a working webhook's. Now creating or changing one needs
// at least one kind, refused in plain words; one kept from before with none
// stays as it was, sent nothing, until an admin ticks one; incoming webhooks
// are untouched.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnOutgoingWebhookIsSentOnlyTheKindsItNames -v

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	webhookController "github.com/akashc777/OneCamp/controllers/Webhook"
)

func TestAnOutgoingWebhookIsSentOnlyTheKindsItNames(t *testing.T) {
	w := newHookWorld(t)
	public := "https://93.184.216.34/hooks"
	const refused = "Choose at least one event to send"

	create := func(t *testing.T, in webhookBusiness.WebhookCreateInput) (int, string, uuid.UUID) {
		t.Helper()
		code, body := w.call(t, "omar", webhookController.HandleCreateWebhook, http.MethodPost, in, uuid.Nil)
		var out struct {
			Webhook struct {
				Id uuid.UUID `json:"id"`
			} `json:"webhook"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		return code, body, out.Webhook.Id
	}
	update := func(t *testing.T, id uuid.UUID, in webhookBusiness.WebhookUpdateInput) (int, string) {
		t.Helper()
		return w.call(t, "omar", webhookController.HandleUpdateWebhook, http.MethodPut, in, id)
	}
	// row is what the webhook's row keeps: its name, and its events as text,
	// "NULL" for none.
	row := func(t *testing.T, id uuid.UUID) (name, events string) {
		t.Helper()
		if err := w.env.PG.QueryRow(`SELECT name, COALESCE(events::text, 'NULL') FROM webhooks WHERE id = $1`, id).Scan(&name, &events); err != nil {
			t.Fatal(err)
		}
		return name, events
	}
	// pointAt sends the webhook's deliveries to the endpoint named name: an
	// address on this machine can't be set through the API.
	pointAt := func(t *testing.T, id uuid.UUID, name string) {
		t.Helper()
		if _, err := w.env.PG.Exec(`UPDATE webhooks SET target_url = $1 WHERE id = $2`, w.servers[name].URL, id); err != nil {
			t.Fatal(err)
		}
	}
	post := func() {
		webhookBusiness.DispatchEvent(context.Background(), "post.created", map[string]interface{}{"channel_id": w.pay.String(), "post_id": uuid.NewString(), "text": "release notes"})
	}

	t.Run("creating an outgoing webhook with no kinds is refused in plain words, and nothing is written", func(t *testing.T) {
		for _, none := range [][]string{nil, {}, {"  "}} {
			in := webhookBusiness.WebhookCreateInput{Name: "No kinds", Type: "outgoing", TargetUrl: &public, Events: none, BotName: "No kinds", ScopeType: "org"}
			if code, body, _ := create(t, in); code != http.StatusBadRequest || !strings.Contains(body, refused) {
				t.Fatalf("events %q: %d %s", none, code, body)
			}
		}
		var n int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM webhooks WHERE name = 'No kinds'`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%d webhooks written (%v), want none", n, err)
		}
	})

	t.Run("a change that names no kinds is refused, and the webhook keeps its own", func(t *testing.T) {
		in := webhookBusiness.WebhookCreateInput{Name: "Deploys", Type: "outgoing", TargetUrl: &public, Events: []string{"post.created"}, BotName: "Deploys", ScopeType: "org"}
		code, body, id := create(t, in)
		if code != http.StatusCreated || id == uuid.Nil {
			t.Fatalf("create with a kind: %d %s", code, body)
		}
		if code, body := update(t, id, webhookBusiness.WebhookUpdateInput{Name: "Deploys, renamed", TargetUrl: &public, IsActive: true, BotName: "Deploys"}); code != http.StatusBadRequest || !strings.Contains(body, refused) {
			t.Fatalf("change with no kinds: %d %s", code, body)
		}
		if name, events := row(t, id); name != "Deploys" || events != `["post.created"]` {
			t.Fatalf("after the refused change: %q, %s; want it as it was", name, events)
		}
	})

	t.Run("a webhook kept from before with no kinds is sent nothing until an admin ticks one", func(t *testing.T) {
		legacy := w.hook(t, "legacy", "omar", "org", nil)
		if _, err := w.env.PG.Exec(`UPDATE webhooks SET events = NULL WHERE id = $1`, legacy); err != nil {
			t.Fatal(err)
		}
		// Made through the API, so the dispatch cache starts afresh.
		code, body, ticked := create(t, webhookBusiness.WebhookCreateInput{Name: "Ticked", Type: "outgoing", TargetUrl: &public, Events: []string{"post.created"}, BotName: "Ticked", ScopeType: "org"})
		if code != http.StatusCreated {
			t.Fatalf("create with a kind: %d %s", code, body)
		}
		w.endpoint(t, "ticked")
		pointAt(t, ticked, "ticked")

		post()
		w.settle(t, legacy, ticked)
		if got := w.sent("legacy"); len(got) != 0 {
			t.Fatalf("the webhook with no kinds was sent %v", got)
		}
		if got := w.sent("ticked"); len(got) != 1 {
			t.Fatalf("the webhook with post.created was sent %v, want the post", got)
		}

		// Only its admin's own choice switches it on: a change that still names
		// no kinds leaves it as it was.
		if code, body := update(t, legacy, webhookBusiness.WebhookUpdateInput{Name: "Legacy, renamed", TargetUrl: &public, IsActive: true, BotName: "legacy"}); code != http.StatusBadRequest || !strings.Contains(body, refused) {
			t.Fatalf("change with no kinds: %d %s", code, body)
		}
		if name, events := row(t, legacy); name != "legacy" || events != "NULL" {
			t.Fatalf("after the refused change: %q, %s; want it as it was", name, events)
		}

		if code, body := update(t, legacy, webhookBusiness.WebhookUpdateInput{Name: "legacy", TargetUrl: &public, Events: []string{"post.created"}, IsActive: true, BotName: "legacy"}); code != http.StatusOK {
			t.Fatalf("change that ticks a kind: %d %s", code, body)
		}
		pointAt(t, legacy, "legacy")
		post()
		w.settle(t, legacy, ticked)
		if got := w.sent("legacy"); len(got) != 1 {
			t.Fatalf("once post.created was ticked, it was sent %v, want the post", got)
		}
	})

	t.Run("an incoming webhook needs no kinds", func(t *testing.T) {
		code, body, id := create(t, webhookBusiness.WebhookCreateInput{Name: "Alerts", Type: "incoming", BotName: "Alerts"})
		if code != http.StatusCreated {
			t.Fatalf("create: %d %s", code, body)
		}
		if code, body := update(t, id, webhookBusiness.WebhookUpdateInput{Name: "Alerts, renamed", IsActive: true, BotName: "Alerts"}); code != http.StatusOK {
			t.Fatalf("change: %d %s", code, body)
		}
		if name, events := row(t, id); name != "Alerts, renamed" || events != "NULL" {
			t.Fatalf("after the change: %q, %s", name, events)
		}
	})
}
