package business

import (
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIMCP"
)

// mergeCatalogStatus must flag an entry installed when a server matches its
// name (case-insensitive, trimmed), and leave the rest not-installed.
func TestMergeCatalogStatus(t *testing.T) {
	entries := []CatalogEntry{
		{Slug: "github", Name: "GitHub"},
		{Slug: "notion", Name: "Notion"},
		{Slug: "linear", Name: "Linear"},
	}
	servers := []*model.McpServer{
		{Name: "  github  "}, // matches "GitHub" case/space-insensitively
		{Name: "Something Else"},
		nil, // nil-safe
	}
	got := mergeCatalogStatus(entries, servers)
	if len(got) != len(entries) {
		t.Fatalf("expected %d entries, got %d", len(entries), len(got))
	}
	byName := map[string]bool{}
	for _, e := range got {
		byName[e.Name] = e.Installed
	}
	if !byName["GitHub"] {
		t.Fatalf("GitHub should be installed")
	}
	if byName["Notion"] || byName["Linear"] {
		t.Fatalf("Notion/Linear should not be installed")
	}
}

// The curated catalog must be internally well-formed: unique slugs/names,
// non-empty required fields, valid transport/auth, and a header name whenever
// header auth is used — so every entry can prefill a valid create request.
func TestConnectorCatalogWellFormed(t *testing.T) {
	slugs := map[string]bool{}
	names := map[string]bool{}
	for _, e := range connectorCatalog {
		if e.Slug == "" || e.Name == "" || e.Description == "" || e.DocsURL == "" {
			t.Fatalf("entry %q has an empty required field", e.Name)
		}
		if slugs[e.Slug] {
			t.Fatalf("duplicate slug %q", e.Slug)
		}
		if names[e.Name] {
			t.Fatalf("duplicate name %q", e.Name)
		}
		slugs[e.Slug] = true
		names[e.Name] = true

		if !model.ValidTransport(e.Transport) {
			t.Fatalf("entry %q has invalid transport %q", e.Name, e.Transport)
		}
		if !model.ValidAuthType(e.AuthType) {
			t.Fatalf("entry %q has invalid auth type %q", e.Name, e.AuthType)
		}
		if e.AuthType == model.AuthHeader && e.AuthHeaderName == "" {
			t.Fatalf("entry %q uses header auth but has no header name", e.Name)
		}
		if e.SecretRequired && e.AuthType == model.AuthNone {
			t.Fatalf("entry %q requires a secret but uses no auth", e.Name)
		}
	}
}
