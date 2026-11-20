//go:build integration

package notification

// The world the send-time re-check's tests run in, shared by the tests in this
// package and those outside it (package notification_test, which may import
// the packages that import this one: business/Doc, whose read rule this
// package is handed when the server starts).

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/akashc777/OneCamp/tests/integration"
)

// Recheck is a database and a graph, with every email the worker sends caught
// rather than sent.
type Recheck struct {
	Ctx  context.Context
	Env  *integration.Env
	DG   *integration.DgraphEnv
	mu   sync.Mutex
	sent []emailService.SendOptions
}

// Person is someone in both stores: their id, their node in the graph and
// their address.
type Person struct {
	ID    uuid.UUID
	UID   string
	Email string
}

// NewRecheck starts the world for t; the send seam is put back when t ends.
func NewRecheck(t *testing.T) *Recheck {
	t.Helper()
	t.Setenv("RESEND_API_KEY", "re_test")
	h := &Recheck{Ctx: context.Background(), Env: integration.SetupEnv(t)}
	if err := postgresInit.ConnectPostgres(h.Ctx, h.Env.DSN); err != nil {
		t.Fatal(err)
	}
	h.DG = integration.SetupDgraph(t) // a group's people, and whether someone is online
	was := sendEmail
	t.Cleanup(func() { sendEmail = was })
	sendEmail = func(_ context.Context, o emailService.SendOptions) (emailService.SendResult, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.sent = append(h.sent, o)
		return emailService.SendResult{MessageID: "msg-" + o.To}, nil
	}
	return h
}

// Exec runs q, failing t when it can't.
func (h *Recheck) Exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := h.Env.PG.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// Person adds someone called name, with an address at example.com.
func (h *Recheck) Person(t *testing.T, name string) Person {
	t.Helper()
	p := Person{ID: uuid.New()}
	handle := name + "-" + p.ID.String()[:8]
	p.Email = handle + "@example.com"
	h.Exec(t, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`, p.ID, p.Email, handle, name)
	h.Exec(t, `INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, $2)`, p.ID, "unsub-"+handle)
	p.UID = h.DG.Mutate(t, map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": p.ID.String(), "user_name": handle})["u"]
	return p
}

// Queued waits for the dispatch and checks each of to has one email waiting.
func (h *Recheck) Queued(t *testing.T, to []Person) {
	t.Helper()
	helpers.SettleBackground(10 * time.Second)
	for _, p := range to {
		var n int
		if err := h.Env.PG.QueryRow(`SELECT COUNT(*) FROM notification_email_queue WHERE user_id = $1 AND status = 'pending'`, p.ID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s's email wasn't queued at the message: %d rows, err %v", p.Email, n, err)
		}
	}
}

// Send runs the worker once as if every waiting notice's window had ended,
// and answers what went to each address.
func (h *Recheck) Send(t *testing.T) map[string][]emailService.SendOptions {
	t.Helper()
	h.Exec(t, `UPDATE notification_email_queue SET next_retry_at = NOW() WHERE status = 'pending' AND next_retry_at > NOW()`)
	return h.SendAsDue(t)
}

// SendAsDue runs the worker once over what is due now, and answers what went
// to each address.
func (h *Recheck) SendAsDue(t *testing.T) map[string][]emailService.SendOptions {
	t.Helper()
	h.mu.Lock()
	h.sent = nil
	h.mu.Unlock()
	processBatch(h.Ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string][]emailService.SendOptions{}
	for _, o := range h.sent {
		out[o.To] = append(out[o.To], o)
	}
	return out
}

// RowSays is what p's latest queue row says.
func (h *Recheck) RowSays(t *testing.T, p Person) (status, why string) {
	t.Helper()
	var last *string
	if err := h.Env.PG.QueryRow(`SELECT status, error_message FROM notification_email_queue WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1`, p.ID).Scan(&status, &last); err != nil {
		t.Fatal(err)
	}
	if last != nil {
		why = *last
	}
	return status, why
}

// NotTold checks p was sent nothing, no email quotes text, and p's row says
// it was suppressed as no longer told.
func (h *Recheck) NotTold(t *testing.T, got map[string][]emailService.SendOptions, p Person, text string) {
	t.Helper()
	if len(got[p.Email]) != 0 {
		t.Errorf("%s was sent %d emails, want none", p.Email, len(got[p.Email]))
	}
	for _, list := range got {
		for _, o := range list {
			if strings.Contains(o.HTML, text) || strings.Contains(o.Text, text) {
				t.Errorf("an email to %s quotes %q", o.To, text)
			}
		}
	}
	if status, why := h.RowSays(t, p); status != "suppressed" || !strings.Contains(why, "no longer told") {
		t.Errorf("%s's row is %s (%q), want suppressed, no longer told", p.Email, status, why)
	}
}
