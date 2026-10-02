package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
)

func requestAs(email string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/connectors/gmail/connect", nil)
	info := models.UserInfo{UserPostgresInfo: models.User{EmailID: email}}
	return r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, info))
}

func TestOnlyTheSharedDemoVisitorIsRefused(t *testing.T) {
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")
	cases := []struct {
		name, demoMode, email string
		refused               bool
	}{
		{"the shared visitor on the demo", "true", "Visitor@Demo.example", true},
		{"the demo's own team on the demo", "true", "owner@company.example", false},
		{"the same address on a normal install", "", "visitor@demo.example", false},
	}
	for _, c := range cases {
		t.Setenv("DEMO_MODE", c.demoMode)
		reached := false
		h := NoPersonalAccountsInDemo(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, requestAs(c.email))
		if refused := w.Code == http.StatusForbidden && !reached; refused != c.refused {
			t.Errorf("%s: refused=%v, want %v", c.name, refused, c.refused)
		}
	}
}
