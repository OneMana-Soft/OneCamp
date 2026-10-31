package business

import (
	"testing"

	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
)

// The "/" menu offered every seeded built-in to everyone, whatever its app's
// state: in a workspace with no Giphy key a member's "/giphy" only said an
// admin had to add one, and a removed Giphy stayed in every menu.
func TestACommandStandsInTheMenuWhereItsAppLetsIt(t *testing.T) {
	giphy := &slashModel.SlashCommand{Command: "giphy", IsBuiltin: true}
	keyed := &appSecretBag{Secrets: map[string]string{"api_key": "k"}}
	installed := func(on, removed bool) *latestApp {
		return &latestApp{App: &slashModel.App{Kind: slashModel.AppKindBuiltin, IsEnabled: on}, Removed: removed}
	}
	cases := []struct {
		name string
		cmd  *slashModel.SlashCommand
		app  *latestApp
		bag  *appSecretBag
		want menuPlace
	}{
		{"Giphy never installed", giphy, nil, nil, menuSetup},
		{"Giphy installed with no key", giphy, installed(true, false), nil, menuSetup},
		{"Giphy installed with an empty key", giphy, installed(true, false), &appSecretBag{Secrets: map[string]string{"api_key": ""}}, menuSetup},
		{"Giphy with its key", giphy, installed(true, false), keyed, menuShown},
		{"Giphy turned off", giphy, installed(false, false), keyed, menuHidden},
		{"Giphy removed", giphy, installed(true, true), keyed, menuHidden},
		{"a built-in no app stands behind", &slashModel.SlashCommand{Command: "remind", IsBuiltin: true}, nil, nil, menuShown},
		{"a custom app's command named like a built-in", &slashModel.SlashCommand{Command: "giphy"}, nil, nil, menuShown},
	}
	for _, c := range cases {
		tpl, ok := commandTemplate(c.cmd)
		if got := commandPlace(tpl, ok, c.app, c.bag); got != c.want {
			t.Errorf("%s: place %d, want %d", c.name, got, c.want)
		}
	}
}

// An app-linked command is weighed by its own app's template; a seeded
// built-in by the built-in template that lists it, never by its name alone.
func TestACommandsTemplateIsItsAppsOrTheBuiltinThatListsIt(t *testing.T) {
	zoom := "zoom"
	if tpl, ok := commandTemplate(&slashModel.SlashCommand{Command: "zoom", AppSlug: &zoom}); !ok || tpl.Slug != "zoom" {
		t.Errorf("an app's command: %q %v, want the Zoom template", tpl.Slug, ok)
	}
	if tpl, ok := commandTemplate(&slashModel.SlashCommand{Command: "giphy", IsBuiltin: true}); !ok || tpl.Slug != giphyAppSlug {
		t.Errorf("the seeded /giphy: %q %v, want the Giphy template", tpl.Slug, ok)
	}
	custom := "acme"
	if _, ok := commandTemplate(&slashModel.SlashCommand{Command: "giphy", AppSlug: &custom}); ok {
		t.Error("a custom app's /giphy took a directory template")
	}
}
