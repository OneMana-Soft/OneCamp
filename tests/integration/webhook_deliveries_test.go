//go:build integration
// +build integration

package integration_test

// Outgoing webhooks are sent their events until they arrive (business/Hooks;
// platform P-API part 2, design/platform/API_CONTRACT.md §3.3), against real
// Postgres 12 with every migration (361 adds the deliveries):
//   - an event is written down before anything is sent, and a failed try waits
//     in Postgres, where any process's drain finds it (a restart loses nothing);
//   - every try and every replay carries the same delivery id and body, signed
//     so the receiving end's own check (VerifySignatureV1) accepts it;
//   - every failed delivery can be sent again in one go;
//   - a paused webhook's deliveries wait and go when it's resumed;
//   - a webhook whose creator left pauses itself, and the admin who resumes it
//     takes it over.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAWebhookDelivery -v

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	hooksBusiness "github.com/akashc777/OneCamp/business/Hooks"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// received is one request the endpoint was sent.
type received struct {
	delivery, retry, timestamp, signature string
	body                                  []byte
}

func TestAWebhookDeliveryArrivesThroughAFailureKeepsItsIDAndWaitsWhilePaused(t *testing.T) {
	t.Setenv("JWT_SECRET", "webhook-deliveries-integration-secret")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	integration.SetupDgraph(t)
	defer hooksBusiness.SendToThisNetworkForTest()()
	db := postgresInit.DBConn.SqlDB

	adminID, _, err := userBusiness.CreateUserWithMethod(ctx, "hooks-admin@example.test", "Hooks Admin", nil, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := userBusiness.CreateAdminUser(ctx, "hooks-admin@example.test"); err != nil {
		t.Fatal(err)
	}

	// The endpoint: fails while failing is above zero, then answers.
	var mu sync.Mutex
	var got []received
	failing := 1
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, received{r.Header.Get("X-OneCamp-Delivery"), r.Header.Get("X-OneCamp-Retry-Num"), r.Header.Get("X-OneCamp-Timestamp"), r.Header.Get("X-OneCamp-Signature"), b})
		fail := failing > 0
		if fail {
			failing--
		}
		mu.Unlock()
		if fail {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer endpoint.Close()
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(got) }
	waitFor := func(n int, why string) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); count() < n; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the endpoint has %d requests, not %d", why, count(), n)
			}
		}
	}
	stays := func(n int, why string) {
		t.Helper()
		time.Sleep(700 * time.Millisecond)
		if c := count(); c != n {
			t.Fatalf("%s: the endpoint has %d requests, not %d", why, c, n)
		}
	}

	// Made as an admin makes it (a public address), then pointed at the endpoint.
	public := "https://93.184.216.34/hooks"
	hook, err := webhookBusiness.CreateWebhook(ctx, webhookBusiness.WebhookCreateInput{
		Name: "Deploys", Type: "outgoing", TargetUrl: &public, Events: []string{"task.created"}, BotName: "Deploys", ScopeType: "org",
	}, adminID)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := webhookBusiness.RegenerateSecret(ctx, hook.Id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE webhooks SET target_url = $1 WHERE id = $2`, endpoint.URL, hook.Id); err != nil {
		t.Fatal(err)
	}
	dispatch := func() {
		webhookBusiness.DispatchEvent(ctx, "task.created", map[string]interface{}{"task_uuid": uuid.NewString()})
	}
	type row struct {
		id       int64
		state    string
		attempts int
		eventID  uuid.UUID
	}
	latest := func() row {
		t.Helper()
		var r row
		if err := db.QueryRowContext(ctx, `SELECT id, state, attempts, event_id FROM webhook_deliveries WHERE webhook_id = $1 ORDER BY id DESC LIMIT 1`, hook.Id).
			Scan(&r.id, &r.state, &r.attempts, &r.eventID); err != nil {
			t.Fatal(err)
		}
		return r
	}

	// 1. The first try fails; the delivery waits in Postgres.
	dispatch()
	waitFor(1, "the first try")
	first := latest()
	if first.state != "pending" || first.attempts != 1 {
		t.Fatalf("after a failed try: %+v", first)
	}

	// 2. Any process's drain sends it once it's due: a restart loses nothing.
	if _, err := db.ExecContext(ctx, `UPDATE webhook_deliveries SET next_at = NOW() WHERE id = $1`, first.id); err != nil {
		t.Fatal(err)
	}
	if err := hooksBusiness.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(2, "the drain's try")
	if r := latest(); r.state != "done" || r.attempts != 2 {
		t.Fatalf("after it arrived: %+v", r)
	}

	// Both tries were the one delivery: its id, its body and a signature the
	// receiving end accepts.
	mu.Lock()
	tries := append([]received(nil), got...)
	mu.Unlock()
	for i, tr := range tries {
		var body struct {
			EventID string `json:"event_id"`
			Event   string `json:"event"`
		}
		if err := json.Unmarshal(tr.body, &body); err != nil {
			t.Fatal(err)
		}
		if tr.delivery != first.eventID.String() || body.EventID != tr.delivery || body.Event != "task.created" {
			t.Fatalf("try %d: delivery %q, body's event id %q, row's %s", i+1, tr.delivery, body.EventID, first.eventID)
		}
		if tr.retry != []string{"0", "1"}[i] {
			t.Fatalf("try %d says retry %q", i+1, tr.retry)
		}
		if !webhookBusiness.VerifySignatureV1(secret, tr.body, tr.timestamp, tr.signature) {
			t.Fatalf("try %d's signature doesn't check out at the receiving end", i+1)
		}
	}
	if string(tries[0].body) != string(tries[1].body) {
		t.Fatal("a retry sent another body")
	}
	var logs, failures int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE NOT success) FROM webhook_logs WHERE webhook_id = $1`, hook.Id).Scan(&logs, &failures); err != nil {
		t.Fatal(err)
	}
	if logs != 2 || failures != 1 {
		t.Fatalf("the webhook's log has %d tries, %d failed", logs, failures)
	}

	// 3. A replay sends the same delivery again.
	if err := hooksBusiness.Replay(ctx, hook.Id, first.id); err != nil {
		t.Fatal(err)
	}
	waitFor(3, "the replay")
	mu.Lock()
	replayed := got[2].delivery
	mu.Unlock()
	if replayed != first.eventID.String() {
		t.Fatalf("the replay carried delivery %q, not %s", replayed, first.eventID)
	}

	// 3b. After an outage, every failed delivery goes again in one go, each
	// with its own event id.
	if _, err := db.ExecContext(ctx, `UPDATE webhook_deliveries SET state = 'dead', finished_at = NOW() WHERE webhook_id = $1`, hook.Id); err != nil {
		t.Fatal(err)
	}
	sent, err := hooksBusiness.ReplayFailed(ctx, hook.Id)
	if err != nil || sent != 1 {
		t.Fatalf("sent %d failed deliveries again, err %v", sent, err)
	}
	waitFor(4, "sending every failed one again")
	mu.Lock()
	again := got[3].delivery
	mu.Unlock()
	if again != first.eventID.String() {
		t.Fatalf("sending all again carried delivery %q, not %s", again, first.eventID)
	}

	// 4. A paused webhook's deliveries wait, and go when it's resumed.
	if err := hooksBusiness.Pause(ctx, hook.Id); err != nil {
		t.Fatal(err)
	}
	dispatch()
	stays(4, "while paused")
	if err := hooksBusiness.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	stays(4, "while paused, after a drain")
	if _, err := hooksBusiness.Resume(ctx, hook.Id, adminID); err != nil {
		t.Fatal(err)
	}
	if err := hooksBusiness.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(5, "after the resume")

	// 5. Its creator leaves: it pauses itself, and the admin who resumes it
	// takes it over.
	if _, _, err := userBusiness.CreateUserWithMethod(ctx, "hooks-second@example.test", "Second Admin", nil, userModels.AuthMethodEmail, false); err != nil {
		t.Fatal(err)
	}
	if err := userBusiness.CreateAdminUser(ctx, "hooks-second@example.test"); err != nil {
		t.Fatal(err)
	}
	var second uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT id FROM users WHERE email_id = $1`, "hooks-second@example.test").Scan(&second); err != nil {
		t.Fatal(err)
	}
	if err := userBusiness.DeactivateUser(ctx, adminID); err != nil {
		t.Fatal(err)
	}
	dispatch()
	stays(5, "with its creator gone")
	var reason string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(paused_reason, '') FROM webhooks WHERE id = $1`, hook.Id).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != hooksBusiness.PausedOwnerGone {
		t.Fatalf("with its creator gone it is paused %q", reason)
	}
	tookOver, err := hooksBusiness.Resume(ctx, hook.Id, second)
	if err != nil || !tookOver {
		t.Fatalf("took over %v, err %v", tookOver, err)
	}
	var owner uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT created_by FROM webhooks WHERE id = $1`, hook.Id).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != second {
		t.Fatalf("it belongs to %s, not the admin who resumed it", owner)
	}
	if err := hooksBusiness.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(6, "after the new owner resumed it")
}
