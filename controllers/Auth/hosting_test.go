package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The sign-in page says where the workspace runs (SHELL-05): "your data stays
// on this server" is true of a self-hosted install, not of OneCamp Cloud and
// not of the public demo, which a stranger shares with every other visitor.
// The public sign-in answer says which it is; the demo comes first, as it is
// never anyone's own server, whatever else the environment says.
func TestHostingAnswersDemoCloudOrSelf(t *testing.T) {
	for _, c := range []struct {
		name, demo, managed, want string
	}{
		{"the public demo", "true", "false", "demo"},
		{"the demo, even marked managed", "true", "true", "demo"},
		{"OneCamp Cloud", "false", "true", "cloud"},
		{"a self-hosted install", "false", "false", "self"},
		{"a self-hosted install that says nothing", "", "false", "self"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("DEMO_MODE", c.demo)
			t.Setenv("ONECAMP_MANAGED", c.managed)
			rec := httptest.NewRecorder()
			GetEnabledProviders(rec, httptest.NewRequest(http.MethodGet, "/auth/providers", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("/auth/providers answered %d: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Hosting string `json:"hosting"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("/auth/providers answered %s", rec.Body.String())
			}
			if body.Hosting != c.want {
				t.Errorf("DEMO_MODE=%q ONECAMP_MANAGED=%q: hosting %q, want %q", c.demo, c.managed, body.Hosting, c.want)
			}
		})
	}
}

// What a stranger learns of where the workspace runs, before signing in: one of
// three words. Never the server's address, the workspace's name, a version or
// any setting, however much of that the server's own config holds; the
// sign-in answer's other fields are pinned in signInAnswer_test.
func TestHostingTellsAStrangerOneOfThreeWordsAndNothingOfTheTenant(t *testing.T) {
	t.Setenv("ORG_NAME", "Kestrel Studio")
	t.Setenv("FE_HOST_DOMAIN", "app.kestrel.example")
	t.Setenv("FRONTEND_DOMAIN", "kestrel.example")
	t.Setenv("BACKEND_DOMAIN", "api.kestrel.example")
	for _, c := range []struct{ demo, managed, want string }{
		{"true", "false", `"demo"`},
		{"false", "true", `"cloud"`},
		{"false", "false", `"self"`},
	} {
		t.Setenv("DEMO_MODE", c.demo)
		t.Setenv("ONECAMP_MANAGED", c.managed)
		rec := httptest.NewRecorder()
		GetEnabledProviders(rec, httptest.NewRequest(http.MethodGet, "/auth/providers", nil))
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("/auth/providers answered %s", rec.Body.String())
		}
		if got := string(body["hosting"]); got != c.want {
			t.Errorf("DEMO_MODE=%s ONECAMP_MANAGED=%s: hosting is %s, want only %s", c.demo, c.managed, got, c.want)
		}
	}
}
