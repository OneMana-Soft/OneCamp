package business

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// auditRecord is one call audit was handed.
type auditRecord struct {
	actor                   *uuid.UUID
	email, kind, action, in string
	meta                    map[string]any
}

// Every write an op makes is in the audit log before it is served (condition
// e; main 12:4x, (b)): who made it, meaning the token's member and the agent
// the token is bound to, the token, the op and the place it names. A read
// isn't recorded, and a write that can't be recorded isn't made, as MCP's
// governed calls aren't ("no record, no action").
func TestAWriteIsRecordedBeforeItIsServedWithWhoMadeIt(t *testing.T) {
	var got []auditRecord
	down := false
	saved := audit
	audit = func(_ context.Context, actor *uuid.UUID, email, kind, action, category, _ string, meta map[string]interface{}) error {
		got = append(got, auditRecord{actor: actor, email: email, kind: kind, action: action, in: category, meta: meta})
		if down {
			return errors.New("the audit log is down")
		}
		return nil
	}
	t.Cleanup(func() { audit = saved })
	savedCharge := chargeRows
	chargeRows = func(context.Context, string, bool, int) error { return nil }
	t.Cleanup(func() { chargeRows = savedCharge })

	served := 0
	serve := func(context.Context, Call) (any, error) {
		served++
		if len(got) != served {
			t.Errorf("served with %d records for %d calls: a write is recorded before it's made", len(got), served)
		}
		return map[string]any{}, nil
	}
	write, read := thing("things.change", Behaviour{Idempotent: true}, serve), thing("things.read", Behaviour{ReadOnly: true}, okServe)

	member := userModels.UserInfo{}
	member.UserPostgresInfo.Id, member.UserPostgresInfo.EmailID = uuid.New(), "ana@example.test"
	token, agent := uuid.NewString(), uuid.NewString()
	plain := context.WithValue(context.Background(), helpers.UserInfoContextKey, member)
	plain = context.WithValue(plain, helpers.ApiTokenIDContextKey, token)
	bound := context.WithValue(plain, helpers.ApiAgentIDContextKey, agent)
	args := map[string]any{"id": "t1"}

	if _, err := invoke(plain, &read, token, args); err != nil || len(got) != 0 {
		t.Fatalf("a read: err %v, %d records; want it served and not recorded", err, len(got))
	}
	for _, c := range []struct {
		ctx       context.Context
		kind      string
		withAgent bool
	}{{plain, "human", false}, {bound, "agent", true}} {
		got = nil
		served = 0
		if _, err := invoke(c.ctx, &write, token, args); err != nil {
			t.Fatalf("a write: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("a write made %d records, want 1", len(got))
		}
		r := got[0]
		if r.actor == nil || *r.actor != member.UserPostgresInfo.Id || r.email != "ana@example.test" || r.kind != c.kind ||
			r.action != "api.things.change" || r.in != "integration" {
			t.Errorf("recorded %v %q %q %q %q, want the member %s, %q, %q, api.things.change, integration",
				r.actor, r.email, r.kind, r.action, r.in, member.UserPostgresInfo.Id, "ana@example.test", c.kind)
		}
		if r.meta["token_id"] != token || r.meta["op"] != "things.change" || r.meta["resource_kind"] != "thing" || r.meta["resource_id"] != "t1" {
			t.Errorf("the record's detail %v doesn't name the token %s, the op and the thing t1", r.meta, token)
		}
		if a, ok := r.meta["agent_id"]; ok != c.withAgent || (ok && a != agent) {
			t.Errorf("the record's agent is %v (present %v), want %v", a, ok, c.withAgent)
		}
	}

	got, served, down = nil, 0, true
	_, err := invoke(plain, &write, token, args)
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusServiceUnavailable || served != 0 {
		t.Errorf("a write the log can't take: err %v, served %d times; want a 503 and nothing written", err, served)
	}
}
