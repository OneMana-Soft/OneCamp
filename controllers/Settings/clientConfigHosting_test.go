package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every signed-in screen can say where the workspace runs, from the same
// answer the sign-in page gets.
func TestClientConfigHosting(t *testing.T) {
	for _, c := range []struct{ demo, managed, want string }{
		{"true", "", "demo"},
		{"", "true", "cloud"},
		{"", "false", "self"},
	} {
		t.Setenv("DEMO_MODE", c.demo)
		t.Setenv("ONECAMP_MANAGED", c.managed)
		w := httptest.NewRecorder()
		GetClientConfig(w, httptest.NewRequest(http.MethodGet, "/config/client", nil))
		var body struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unreadable answer: %v", err)
		}
		if body.Data["hosting"] != c.want {
			t.Errorf("DEMO_MODE=%q ONECAMP_MANAGED=%q: hosting %v, want %q", c.demo, c.managed, body.Data["hosting"], c.want)
		}
	}
}
