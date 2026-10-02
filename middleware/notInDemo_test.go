package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPersonalAccountsAreRefusedOnlyOnTheDemo(t *testing.T) {
	reached := false
	h := NoPersonalAccountsInDemo(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))

	t.Setenv("DEMO_MODE", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/connectors/gmail/connect", nil))
	if w.Code != http.StatusForbidden || reached {
		t.Fatalf("demo: status %d, reached=%v", w.Code, reached)
	}

	t.Setenv("DEMO_MODE", "")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/connectors/gmail/connect", nil))
	if !reached {
		t.Fatal("a normal install was refused")
	}
}
