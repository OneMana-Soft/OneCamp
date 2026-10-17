package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GET /auth/providers answers anyone, signed in or not (router/publicSurface_test
// lists it), so everything in it is something a stranger at the sign-in page
// may read. Since 15 Oct 2026 it names the workspace too (design review
// PG1-01): the sign-in page and the boot screen have no session to ask
// /config/client with, and a self-hosted workspace renamed on its server
// should read its new name there as well.
//
// A field added to the answer goes on this list, with why a stranger may read
// it, or this test fails.
var signInAnswerFields = map[string]string{
	"status":           "the answer's own outcome",
	"providers":        "which sign-in buttons to draw: each method on or off, never a setting behind it",
	"email_configured": "whether mailed links (a password reset, an invitation) can arrive, so the page doesn't offer one in vain",
	"email_sender":     "the address mail comes from, which every mail it sends shows anyway",
	"workspace_name":   "the workspace's name, which its own sign-in page shows",
}

func TestTheSignInAnswerCarriesOnlyPublicFields(t *testing.T) {
	answer := func() map[string]json.RawMessage {
		t.Helper()
		rec := httptest.NewRecorder()
		GetEnabledProviders(rec, httptest.NewRequest(http.MethodGet, "/auth/providers", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("/auth/providers answered %d: %s", rec.Code, rec.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("/auth/providers answered %s", rec.Body.String())
		}
		return body
	}

	t.Setenv("ORG_NAME", "  Kestrel \n Studio ")
	body := answer()
	for field := range body {
		if _, public := signInAnswerFields[field]; !public {
			t.Errorf("/auth/providers carries %q, which nobody has said a stranger may read", field)
		}
	}
	for field := range signInAnswerFields {
		if _, ok := body[field]; !ok {
			t.Errorf("/auth/providers no longer carries %q: take it off signInAnswerFields", field)
		}
	}

	// Each sign-in method is on or off, and nothing more about it.
	var providers map[string]any
	if err := json.Unmarshal(body["providers"], &providers); err != nil || len(providers) == 0 {
		t.Fatalf("providers: %s", body["providers"])
	}
	for method, on := range providers {
		if _, ok := on.(bool); !ok {
			t.Errorf("providers[%q] is %s, not on or off", method, body["providers"])
		}
	}

	// The name as the server goes by it, as one line; "" when nobody has named it.
	for _, c := range []struct{ env, want string }{
		{"  Kestrel \n Studio ", "Kestrel Studio"},
		{"", ""},
	} {
		t.Setenv("ORG_NAME", c.env)
		var name string
		if err := json.Unmarshal(answer()["workspace_name"], &name); err != nil || name != c.want {
			t.Errorf("ORG_NAME %q: workspace_name %q (%v), want %q", c.env, name, err, c.want)
		}
	}
}
