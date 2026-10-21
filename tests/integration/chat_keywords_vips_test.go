//go:build integration
// +build integration

package integration_test

// A person's keywords and VIPs (chat P3 stage 3), against real Postgres 12,
// Redis and Dgraph: each keeps their own through /me/keywords and /me/vips;
// a channel post that holds someone's keyword as a whole word tells them as
// a mention would; a VIP's words reach someone through their pause.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestKeywordsAndVIPs' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	personalAlertBusiness "github.com/akashc777/OneCamp/business/PersonalAlert"
	conversationController "github.com/akashc777/OneCamp/controllers/Conversation"
)

func TestKeywordsAndVIPs(t *testing.T) {
	w := newReachWorld(t)
	maya, jonas, ada := w.as(t, "maya", false), w.as(t, "jonas", false), w.as(t, "ada", false)
	list := func(ctx context.Context, handler http.HandlerFunc, method string, body any) (int, []string, string) {
		t.Helper()
		code, data, msg := w.run(ctx, handler, method, "", body)
		var out []string
		if code == http.StatusOK {
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatalf("list %s: %v", data, err)
			}
		}
		return code, out, msg
	}

	// Keywords: trimmed, each once in any case; a bad one refused in words.
	code, kws, msg := list(ada, conversationController.PutMyKeywords, http.MethodPut,
		map[string]any{"keywords": []string{" Budget ", "budget", "q4 plan"}})
	if code != http.StatusOK || !slices.Equal(kws, []string{"Budget", "q4 plan"}) {
		t.Fatalf("Ada's keywords: %d %v %s", code, kws, msg)
	}
	if code, _, msg := list(ada, conversationController.PutMyKeywords, http.MethodPut,
		map[string]any{"keywords": []string{"x"}}); code != http.StatusBadRequest || msg != "A keyword is 2 to 64 characters." {
		t.Errorf("a one-letter keyword: %d %q", code, msg)
	}
	if _, kws, _ := list(ada, conversationController.GetMyKeywords, http.MethodGet, nil); !slices.Equal(kws, []string{"Budget", "q4 plan"}) {
		t.Errorf("Ada's keywords read back: %v (a refused change must keep them)", kws)
	}
	if _, kws, _ := list(jonas, conversationController.GetMyKeywords, http.MethodGet, nil); len(kws) != 0 {
		t.Errorf("Jonas sees keywords he never set: %v", kws)
	}

	// A post that holds "budget" as a word tells Ada, whose level for #launch
	// is mentions only; one that holds it inside a word doesn't.
	code, said, msg := w.post(t, maya, "<p>The BUDGET is ready</p>")
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("Maya's post: %d %s", code, msg)
	}
	if got := w.pushedFor(t, said, []string{"ada"}); !slices.Equal(got, []string{"ada"}) {
		t.Errorf("pushed of the budget post: %v, want Ada alone", got)
	}
	_, said, _ = w.post(t, maya, "<p>Budgetary matters</p>")
	if got := w.pushedFor(t, said, nil); len(got) != 0 {
		t.Errorf("pushed of a post with the keyword inside a word: %v", got)
	}

	// VIPs: live people only, never oneself; an id that isn't one is dropped.
	vipList := func(handler http.HandlerFunc, method string, body any) (int, []personalAlertBusiness.VIP, string) {
		t.Helper()
		code, data, msg := w.run(jonas, handler, method, "", body)
		var out []personalAlertBusiness.VIP
		if code == http.StatusOK {
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatalf("VIPs %s: %v", data, err)
			}
		}
		return code, out, msg
	}
	want := []personalAlertBusiness.VIP{{UUID: w.people["maya"].String(), Name: "maya"}, {UUID: w.people["zed"].String(), Name: "zed"}}
	code, vips, msg := vipList(conversationController.PutMyVIPs, http.MethodPut, map[string]any{"vips": []string{
		w.people["maya"].String(), w.people["jonas"].String(), uuid.NewString(), "junk", w.people["zed"].String()}})
	if code != http.StatusOK || !slices.Equal(vips, want) {
		t.Fatalf("Jonas's VIPs: %d %v %s", code, vips, msg)
	}
	if _, vips, _ := vipList(conversationController.GetMyVIPs, http.MethodGet, nil); !slices.Equal(vips, want) {
		t.Errorf("Jonas's VIPs read back: %v", vips)
	}

	// Jonas pauses and keeps the keyword "deadline". Sam's post with it
	// waits for his pause; Maya's, a VIP's, reaches him through it.
	if code, _, msg := list(jonas, conversationController.PutMyKeywords, http.MethodPut,
		map[string]any{"keywords": []string{"deadline"}}); code != http.StatusOK {
		t.Fatalf("Jonas's keywords: %d %s", code, msg)
	}
	for _, q := range []string{
		`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, $2) ON CONFLICT (user_id) DO NOTHING`,
		`UPDATE users_notification_preferences SET notifications_paused_until = NOW() + INTERVAL '1 hour' WHERE user_id = $1`,
	} {
		args := []any{w.people["jonas"]}
		if len(q) > 0 && q[0] == 'I' {
			args = append(args, uuid.NewString())
		}
		if _, err := w.env.PG.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	_, said, _ = w.post(t, w.as(t, "sam", false), "<p>The deadline moved</p>")
	if got := w.pushedFor(t, said, nil); slices.Contains(got, "jonas") {
		t.Errorf("Sam's post reached Jonas through his pause: %v", got)
	}
	_, said, _ = w.post(t, maya, "<p>The deadline is Friday</p>")
	if got := w.pushedFor(t, said, []string{"jonas"}); !slices.Contains(got, "jonas") {
		t.Errorf("Maya, his VIP, didn't reach Jonas through his pause: %v", got)
	}
}
