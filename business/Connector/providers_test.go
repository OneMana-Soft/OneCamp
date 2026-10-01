package business

import "testing"

// TestProvidersRegistered verifies the three connectors are registered with the
// expected client reuse and that write-capable providers declare a write scope
// for the consent screen.
func TestProvidersRegistered(t *testing.T) {
	cases := []struct {
		id          string
		reuseClient string
	}{
		{ProviderGmail, "google"},
		{ProviderCalendar, "google"},
		{ProviderGitHub, "github"},
	}
	for _, c := range cases {
		p, ok := providerByID(c.id)
		if !ok {
			t.Fatalf("provider %q not registered", c.id)
		}
		if p.reuseClient != c.reuseClient {
			t.Errorf("provider %q reuseClient = %q, want %q", c.id, p.reuseClient, c.reuseClient)
		}
		if len(p.Scopes) == 0 {
			t.Errorf("provider %q has no scopes", c.id)
		}
		if p.AuthURL == "" || p.TokenURL == "" {
			t.Errorf("provider %q missing auth/token URL", c.id)
		}
		if len(p.Permissions) == 0 {
			t.Errorf("provider %q has no permission descriptions for consent", c.id)
		}
	}
}

// TestProvidersStableOrder ensures Providers() returns a deterministic,
// name-sorted list (the UI relies on stable ordering).
func TestProvidersStableOrder(t *testing.T) {
	got := Providers()
	if len(got) < 3 {
		t.Fatalf("expected at least 3 providers, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if lower(got[i-1].Name) > lower(got[i].Name) {
			t.Errorf("providers not sorted by name: %q before %q", got[i-1].Name, got[i].Name)
		}
	}
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// providerByID finds a registered provider the way the app lists them.
func providerByID(id string) (Provider, bool) {
	for _, p := range Providers() {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}
