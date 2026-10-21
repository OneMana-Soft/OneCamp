//go:build integration
// +build integration

package integration_test

// A channel's broadcast rules for the composer (chat P3), against real
// Postgres 12, Redis and Dgraph: what it offers and asks before a broadcast,
// with how many each would notify now, and the channel's own "Allow @here and
// @channel", which only its managers change, in the app, and which the audit
// log records.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAChannelsBroadcastRules' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	reachBusiness "github.com/akashc777/OneCamp/business/Reach"
	conversationController "github.com/akashc777/OneCamp/controllers/Conversation"
)

func TestAChannelsBroadcastRules(t *testing.T) {
	w := newReachWorld(t)
	route := func(ctx context.Context, kind, id string) context.Context {
		rc := chi.NewRouteContext()
		rc.URLParams.Add("kind", kind)
		rc.URLParams.Add("id", id)
		return context.WithValue(ctx, chi.RouteCtxKey, rc)
	}
	launch := w.channel.String()
	rules := func(ctx context.Context, method, query string, body any) (int, reachBusiness.Rules, string) {
		t.Helper()
		handler := conversationController.GetBroadcastRules
		if method == http.MethodPut {
			handler = conversationController.PutBroadcastRules
		}
		code, data, msg := w.run(route(ctx, "channel", launch), handler, method, query, body)
		var r reachBusiness.Rules
		if code == http.StatusOK {
			if err := json.Unmarshal(data, &r); err != nil {
				t.Fatalf("rules %s: %v", data, err)
			}
		}
		return code, r, msg
	}
	maya, jonas := w.as(t, "maya", false), w.as(t, "jonas", false)

	// Maya, a member: she may; #launch's five are below the question's six;
	// @channel would notify three (never her or the agent), @here one.
	code, r, msg := rules(maya, http.MethodGet, "?reach=channel,here", nil)
	if code != http.StatusOK || !r.May || r.Members != 5 || r.ConfirmAt != 6 || r.Manages || !r.MembersMay || r.Own {
		t.Fatalf("Maya's rules: %d %+v %s", code, r, msg)
	}
	if r.Reach[reachBusiness.Channel] != 3 || r.Reach[reachBusiness.Here] != 1 {
		t.Errorf("reach %v: @channel three, @here one", r.Reach)
	}

	// Only a manager changes the channel's setting, and never through a token.
	if code, _, msg := rules(maya, http.MethodPut, "", map[string]any{"members": false}); code != http.StatusForbidden || msg != "Only the channel's managers can change who notifies all of it." {
		t.Errorf("Maya changing it: %d %q", code, msg)
	}
	if code, _, _ := rules(w.as(t, "jonas", true), http.MethodPut, "", map[string]any{"members": false}); code != http.StatusForbidden {
		t.Errorf("Jonas changing it through a token: %d", code)
	}
	code, r, msg = rules(jonas, http.MethodPut, "", map[string]any{"members": false})
	if code != http.StatusOK || r.MembersMay || !r.Own || !r.Manages || !r.May {
		t.Fatalf("Jonas turning it off: %d %+v %s", code, r, msg)
	}
	var recorded int
	if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE action = 'policy.changed' AND metadata->>'scope_id' = $1`, launch).Scan(&recorded); err != nil || recorded != 1 {
		t.Errorf("%d audit rows for the change (%v)", recorded, err)
	}
	if _, r, _ := rules(maya, http.MethodGet, "", nil); r.May || r.Why != "Only #launch's managers can notify everyone in it." {
		t.Errorf("Maya's rules where members may not: %+v", r)
	}
	// null gives it back to the workspace's.
	if code, r, _ := rules(jonas, http.MethodPut, "", map[string]any{"members": nil}); code != http.StatusOK || !r.MembersMay || r.Own {
		t.Errorf("giving it back: %d %+v", code, r)
	}

	// Not a channel, a channel she isn't in, or no channel at all.
	if code, _, _ := w.run(route(maya, "dm", launch), conversationController.GetBroadcastRules, http.MethodGet, "", nil); code != http.StatusNotFound {
		t.Errorf("a DM's broadcast rules: %d", code)
	}
	if code, _, _ := w.run(route(w.as(t, "zed", false), "channel", launch), conversationController.GetBroadcastRules, http.MethodGet, "", nil); code != http.StatusForbidden {
		t.Errorf("Zed, not in the public #launch: %d", code)
	}
	if code, _, _ := w.run(route(maya, "channel", "not-an-id"), conversationController.GetBroadcastRules, http.MethodGet, "", nil); code != http.StatusBadRequest {
		t.Errorf("a bad id: %d", code)
	}
}
