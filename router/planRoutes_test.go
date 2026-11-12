package router

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// One answer about the plan: /admin/seats, which older web builds read, is
// the same handler as /admin/plan, and prices sit behind the admin-or-demo-
// visitor gate, never the member-wide config routes.
func TestSeatsIsTheSameAnswerAsPlan(t *testing.T) {
	handlers := map[string]string{}
	chains := map[string][]string{}
	err := chi.Walk(routerMux(), func(method, pattern string, h http.Handler, mws ...func(http.Handler) http.Handler) error {
		if method != http.MethodGet {
			return nil
		}
		handlers[pattern] = funcName(h)
		for _, mw := range mws {
			chains[pattern] = append(chains[pattern], funcName(mw))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	const plan = "controllers/AdminConfig.GetPlan"
	for _, p := range []string{"/admin/plan", "/admin/seats"} {
		if handlers[p] != plan {
			t.Errorf("GET %s is answered by %q, want %s", p, handlers[p], plan)
		}
	}
	if handlers["/plan/prices"] != "controllers/AdminConfig.GetPlanPrices" {
		t.Errorf("GET /plan/prices is answered by %q", handlers["/plan/prices"])
	}
	chain := strings.Join(chains["/plan/prices"], " ")
	if !strings.Contains(chain, "middleware.VerifyAuth") || !strings.Contains(chain, "middleware.AdminOrDemoVisitor") {
		t.Errorf("GET /plan/prices must be signed in and an admin's or the demo visitor's: %s", chain)
	}
}
