//go:build integration
// +build integration

package integration_test

// An outgoing webhook sends only what its owner may read (audit A9, package
// 21), against real Postgres 12 and Dgraph. Before, an admin outside a private
// channel could set a webhook up for it and be sent its messages, a workspace
// webhook was sent every project's tasks whatever its owner's role, an admin
// could point another admin's webhook at their own server, and a webhook's log
// showed every admin what it sent.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAWebhookSendsOnlyWhatItsOwnerMayRead -v

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	hooksBusiness "github.com/akashc777/OneCamp/business/Hooks"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	webhookController "github.com/akashc777/OneCamp/controllers/Webhook"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// hookWorld is a workspace with two admins: Omar is in the private channel
// "pay" and views the project "Launch"; Ines is in neither. Each endpoint keeps
// what it was sent.
type hookWorld struct {
	env     *integration.Env
	people  map[string]uuid.UUID
	pay     uuid.UUID
	launch  uuid.UUID
	mu      sync.Mutex
	got     map[string][]string
	servers map[string]*httptest.Server
}

func newHookWorld(t *testing.T) *hookWorld {
	t.Helper()
	t.Setenv("JWT_SECRET", "webhook-reach-integration-secret")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	t.Cleanup(hooksBusiness.SendToThisNetworkForTest())

	w := &hookWorld{env: env, people: map[string]uuid.UUID{}, pay: uuid.New(), launch: uuid.New(), got: map[string][]string{}, servers: map[string]*httptest.Server{}}
	live := "0001-01-01T00:00:00Z"
	var nodes []map[string]any
	for _, n := range []string{"omar", "ines"} {
		w.people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, is_admin, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, true, NOW(), NOW())`, w.people[n], n); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": w.people[n].String(), "user_name": n, "user_deleted_at": live})
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'pay', true)`, []any{w.pay}},
		{`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $2)`, []any{w.launch, w.people["omar"]}},
		{`INSERT INTO project_roles (project_id, user_id, role) VALUES ($1, $2, 'viewer')`, []any{w.launch, w.people["omar"]}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	nodes = append(nodes,
		map[string]any{"uid": "_:pay", "dgraph.type": "Channel", "ch_uuid": w.pay.String(), "ch_name": "pay", "ch_private": true,
			"ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:omar"}}},
		map[string]any{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": w.launch.String(), "project_name": "Launch",
			"project_deleted_at": live, "project_members": []map[string]any{{"uid": "_:omar"}}},
	)
	dg.Mutate(t, nodes)
	return w
}

// endpoint is a server that keeps the bodies it is sent, by name.
func (w *hookWorld) endpoint(t *testing.T, name string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		w.got[name] = append(w.got[name], string(b))
		w.mu.Unlock()
		rw.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.Close)
	w.servers[name] = s
	return s.URL
}

func (w *hookWorld) sent(name string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.got[name]...)
}

// as is a request context signed in as name.
func (w *hookWorld) as(t *testing.T, name string) context.Context {
	t.Helper()
	ctx := context.Background()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, w.people[name])
	if err != nil || pg == nil {
		t.Fatalf("load %s: %v", name, err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, w.people[name].String())
	if err != nil || gr == nil {
		t.Fatalf("load %s's node: %v", name, err)
	}
	return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
}

// call runs handler as name with body and the webhook's id, and answers its
// status and body.
func (w *hookWorld) call(t *testing.T, name string, handler http.HandlerFunc, method string, body any, webhookID uuid.UUID) (int, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	ctx := w.as(t, name)
	if webhookID != uuid.Nil {
		rc := chi.NewRouteContext()
		rc.URLParams.Add("webhookId", webhookID.String())
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rc)
	}
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(method, "/", bytes.NewReader(raw)).WithContext(ctx))
	return rec.Code, rec.Body.String()
}

// hook writes a webhook as one set up before the fix would be, owned by owner,
// pointed at the endpoint named name.
func (w *hookWorld) hook(t *testing.T, name, owner, scope string, entity *uuid.UUID, events ...string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	evs, _ := json.Marshal(events)
	if _, err := w.env.PG.Exec(`INSERT INTO webhooks (id, name, type, token, secret, target_url, events, is_active, created_by, bot_name, scope_type, scope_entity_id, created_at, updated_at)
		VALUES ($1, $2, 'outgoing', $3, 'shh', $4, $5, true, $6, $2, $7, $8, NOW(), NOW())`,
		id, name, uuid.NewString(), w.endpoint(t, name), string(evs), w.people[owner], scope, entity); err != nil {
		t.Fatal(err)
	}
	return id
}

// settle sends what is due and waits until no delivery of the webhooks is left
// to try now.
func (w *hookWorld) settle(t *testing.T, hooks ...uuid.UUID) {
	t.Helper()
	ids := make([]string, len(hooks))
	for i, h := range hooks {
		ids[i] = h.String()
	}
	for deadline := time.Now().Add(20 * time.Second); ; {
		if err := hooksBusiness.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
		var due int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries d JOIN webhooks wh ON wh.id = d.webhook_id
			WHERE d.webhook_id::text = ANY(string_to_array($1, ',')) AND d.state = 'pending' AND wh.paused_at IS NULL`, strings.Join(ids, ",")).Scan(&due); err != nil {
			t.Fatal(err)
		}
		if due == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d deliveries still due", due)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (w *hookWorld) pausedFor(t *testing.T, hook uuid.UUID) string {
	t.Helper()
	var why *string
	if err := w.env.PG.QueryRow(`SELECT paused_reason FROM webhooks WHERE id = $1`, hook).Scan(&why); err != nil {
		t.Fatal(err)
	}
	if why == nil {
		return ""
	}
	return *why
}

func TestAWebhookSendsOnlyWhatItsOwnerMayRead(t *testing.T) {
	w := newHookWorld(t)
	public := "https://93.184.216.34/hooks"

	t.Run("an admin outside a private channel can't set a webhook up for it", func(t *testing.T) {
		in := webhookBusiness.WebhookCreateInput{Name: "Pay", Type: "outgoing", TargetUrl: &public, Events: []string{"post.created"}, BotName: "Pay", ScopeType: "channel", ScopeEntityId: &w.pay}
		if code, body := w.call(t, "ines", webhookController.HandleCreateWebhook, http.MethodPost, in, uuid.Nil); code != http.StatusBadRequest || !strings.Contains(body, "You aren't in that channel") {
			t.Fatalf("Ines, outside #pay: %d %s", code, body)
		}
		if code, body := w.call(t, "omar", webhookController.HandleCreateWebhook, http.MethodPost, in, uuid.Nil); code != http.StatusCreated {
			t.Fatalf("Omar, in #pay: %d %s", code, body)
		}
	})

	t.Run("a private channel's message reaches only webhooks whose owners are in it", func(t *testing.T) {
		inesPay := w.hook(t, "ines-pay", "ines", "channel", &w.pay, "post.created")
		inesAll := w.hook(t, "ines-all", "ines", "org", nil, "post.created")
		omarPay := w.hook(t, "omar-pay", "omar", "channel", &w.pay, "post.created")
		omarAll := w.hook(t, "omar-all", "omar", "org", nil, "post.created")
		webhookBusiness.DispatchEvent(context.Background(), "post.created", map[string]interface{}{"channel_id": w.pay.String(), "post_id": uuid.NewString(), "text": "salaries"})
		w.settle(t, inesPay, inesAll, omarPay, omarAll)

		if got := w.sent("ines-pay"); len(got) != 0 {
			t.Fatalf("Ines's webhook for #pay was sent %v", got)
		}
		if why := w.pausedFor(t, inesPay); why != "owner_cant_read" {
			t.Fatalf("Ines's webhook for #pay is paused for %q, want owner_cant_read", why)
		}
		if got := w.sent("ines-all"); len(got) != 0 {
			t.Fatalf("Ines's workspace webhook was sent %v", got)
		}
		if got := w.sent("omar-pay"); len(got) != 1 || !strings.Contains(got[0], "salaries") {
			t.Fatalf("Omar's webhook for #pay was sent %v", got)
		}
		if got := w.sent("omar-all"); len(got) != 1 || strings.Contains(got[0], "salaries") {
			t.Fatalf("Omar's workspace webhook was sent %v, want the ids", got)
		}

		// The log of Omar's webhook shows Ines its ids, and Omar the message.
		code, body := w.call(t, "ines", webhookController.HandleGetWebhookLogs, http.MethodGet, nil, omarPay)
		if code != http.StatusOK || strings.Contains(body, "salaries") || !strings.Contains(body, "post_id") {
			t.Fatalf("Ines reading Omar's log: %d %s", code, body)
		}
		if code, body := w.call(t, "omar", webhookController.HandleGetWebhookLogs, http.MethodGet, nil, omarPay); code != http.StatusOK || !strings.Contains(body, "salaries") {
			t.Fatalf("Omar reading his log: %d %s", code, body)
		}
	})

	t.Run("a workspace webhook is sent a project's task only when its owner has a role there", func(t *testing.T) {
		inesTasks := w.hook(t, "ines-tasks", "ines", "org", nil, "task.created")
		omarTasks := w.hook(t, "omar-tasks", "omar", "org", nil, "task.created")
		webhookBusiness.DispatchEvent(context.Background(), "task.created", map[string]interface{}{"project_id": w.launch.String(), "task_id": uuid.NewString(), "name": "Price the launch"})
		w.settle(t, inesTasks, omarTasks)
		if got := w.sent("ines-tasks"); len(got) != 0 {
			t.Fatalf("Ines, with no role in Launch, was sent %v", got)
		}
		if got := w.sent("omar-tasks"); len(got) != 1 || !strings.Contains(got[0], "Price the launch") {
			t.Fatalf("Omar, a viewer, was sent %v", got)
		}
	})

	t.Run("an admin who points another's webhook at their own server takes it over", func(t *testing.T) {
		shared := w.hook(t, "omar-shared", "omar", "org", nil, "task.created")
		elsewhere := "https://93.184.216.34/ines"
		in := webhookBusiness.WebhookUpdateInput{Name: "Shared", TargetUrl: &elsewhere, Events: []string{"task.created"}, IsActive: true, BotName: "Shared"}
		if code, body := w.call(t, "ines", webhookController.HandleUpdateWebhook, http.MethodPut, in, shared); code != http.StatusOK {
			t.Fatalf("Ines pointing it at her server: %d %s", code, body)
		}
		var owner uuid.UUID
		if err := w.env.PG.QueryRow(`SELECT created_by FROM webhooks WHERE id = $1`, shared).Scan(&owner); err != nil || owner != w.people["ines"] {
			t.Fatalf("it belongs to %s (%v), want Ines", owner, err)
		}
		// Her server, standing in for the address she gave.
		if _, err := w.env.PG.Exec(`UPDATE webhooks SET target_url = $1 WHERE id = $2`, w.endpoint(t, "ines-server"), shared); err != nil {
			t.Fatal(err)
		}
		webhookBusiness.DispatchEvent(context.Background(), "task.created", map[string]interface{}{"project_id": w.launch.String(), "task_id": uuid.NewString(), "name": "Price the launch"})
		w.settle(t, shared)
		if got := w.sent("ines-server"); len(got) != 0 {
			t.Fatalf("Ines's server was sent Omar's project: %v", got)
		}
	})
}
