package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
)

func pricesRequestAs(email string, admin bool) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/plan/prices", nil)
	info := models.UserInfo{UserPostgresInfo: models.User{EmailID: email, IsAdmin: admin}}
	return r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, info))
}

// A member who isn't an admin never makes this server ask onemana.dev for
// anything: the server reaches out only when an admin acts.
func TestPricesRefuseAMember(t *testing.T) {
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")
	for _, c := range []struct{ name, demoMode, email string }{
		{"a member on a normal install", "", "member@company.example"},
		{"a member on the demo who isn't the shared visitor", "true", "member@company.example"},
		{"the visitor's address on a normal install", "", "visitor@demo.example"},
	} {
		t.Setenv("DEMO_MODE", c.demoMode)
		reached := false
		h := AdminOrDemoVisitor(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, pricesRequestAs(c.email, false))
		var body struct{ Code string }
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if reached || w.Code != http.StatusForbidden || body.Code != AdminRequired {
			t.Errorf("%s: reached=%v status=%d code=%q, want a 403 %s", c.name, reached, w.Code, body.Code, AdminRequired)
		}
	}

	h := AdminOrDemoVisitor(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/plan/prices", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("nobody signed in: %d, want 401", w.Code)
	}
}

// The demo's shared visitor sees prices on the demo's plan card, as an admin
// does on their own server.
func TestPricesAllowTheDemoVisitor(t *testing.T) {
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")
	for _, c := range []struct {
		name, demoMode, email string
		admin                 bool
	}{
		{"the shared visitor on the demo", "true", "Visitor@Demo.example", false},
		{"an admin on a normal install", "", "owner@company.example", true},
		{"an admin on the demo", "true", "owner@company.example", true},
	} {
		t.Setenv("DEMO_MODE", c.demoMode)
		reached := false
		h := AdminOrDemoVisitor(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, pricesRequestAs(c.email, c.admin))
		if !reached {
			t.Errorf("%s: refused with %d", c.name, w.Code)
		}
	}
}
