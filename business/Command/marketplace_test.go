package business

import (
	"testing"

	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
)

// TestMarketplaceTemplatesValid asserts every curated template is internally
// consistent so a one-click install always produces a valid app.
func TestMarketplaceTemplatesValid(t *testing.T) {
	seen := map[string]bool{}
	for _, tpl := range marketplaceTemplates {
		if tpl.Slug == "" || tpl.Name == "" {
			t.Errorf("template missing slug/name: %+v", tpl)
		}
		if seen[tpl.Slug] {
			t.Errorf("duplicate template slug: %s", tpl.Slug)
		}
		seen[tpl.Slug] = true

		if tpl.Kind != slashModel.AppKindExternal && tpl.Kind != slashModel.AppKindOAuth && tpl.Kind != slashModel.AppKindBuiltin {
			t.Errorf("template %s has invalid kind %q", tpl.Slug, tpl.Kind)
		}
		if len(tpl.Commands) == 0 {
			t.Errorf("template %s provides no commands", tpl.Slug)
		}
		// OAuth apps must declare provider URLs so install pre-fills them.
		if tpl.Kind == slashModel.AppKindOAuth {
			if tpl.OAuthConfig == nil || tpl.OAuthConfig.AuthURL == "" || tpl.OAuthConfig.TokenURL == "" {
				t.Errorf("oauth template %s missing auth/token URL", tpl.Slug)
			}
		}
		// Every required setup field must have a key/label/type.
		for _, f := range tpl.Setup {
			if f.Key == "" || f.Label == "" || f.Type == "" {
				t.Errorf("template %s has an incomplete setup field: %+v", tpl.Slug, f)
			}
		}
	}
}

// TestGetTemplate verifies lookup by slug.
func TestGetTemplate(t *testing.T) {
	if _, ok := getTemplate("giphy"); !ok {
		t.Error("expected giphy template to exist")
	}
	if _, ok := getTemplate("nonexistent-app"); ok {
		t.Error("did not expect nonexistent template")
	}
}

// TestBuiltinTemplatesHaveHandlers asserts every built-in app template's
// commands are actually wired to an in-process handler. A built-in app that
// declares a command with no registered handler would resolve at runtime to
// "registered but unavailable" — this catches that drift at test time.
func TestBuiltinTemplatesHaveHandlers(t *testing.T) {
	for _, tpl := range marketplaceTemplates {
		if tpl.Kind != slashModel.AppKindBuiltin {
			continue
		}
		if len(tpl.Commands) == 0 {
			t.Errorf("builtin template %s declares no commands", tpl.Slug)
		}
		for _, c := range tpl.Commands {
			name := normalizeCommand(c.Command)
			if _, ok := commandRegistry[name]; !ok {
				t.Errorf("builtin template %s command /%s has no registered handler", tpl.Slug, c.Command)
			}
		}
	}
}

// TestNoCommandCollisionBetweenBuiltinsAndTemplates guards the exact bug that
// made Giphy show "No commands yet": a marketplace command that is also a
// seeded org-scoped built-in MUST be a built-in app (so install skips creating
// an app-linked row that would collide on the unique (command, scope) index).
func TestNoCommandCollisionBetweenBuiltinsAndTemplates(t *testing.T) {
	builtinCmds := map[string]bool{}
	for _, b := range builtinCatalog {
		builtinCmds[normalizeCommand(b.command)] = true
	}
	for _, tpl := range marketplaceTemplates {
		if tpl.Kind == slashModel.AppKindBuiltin {
			continue // built-in apps intentionally reuse seeded commands
		}
		for _, c := range tpl.Commands {
			if builtinCmds[normalizeCommand(c.Command)] {
				t.Errorf("external/oauth template %s declares /%s which collides with a seeded built-in; either rename it or make the template kind=builtin",
					tpl.Slug, c.Command)
			}
		}
	}
}
