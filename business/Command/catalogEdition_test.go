package business

import (
	"regexp"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
)

// The edition without AI seeded /ask, "Ask OneCamp AI a quick question", into
// every "/" menu with no handler for it in the build: the command was offered
// and only answered "registered but unavailable" (23 Oct 2026). A command
// OneCamp seeds, and a built-in app's own command, has its handler in the
// build it ships in, on every line.
func TestEverySeededCommandHasItsHandler(t *testing.T) {
	for _, b := range builtinCatalog {
		if _, ok := commandRegistry[b.command]; !ok {
			t.Errorf("/%s is seeded into every \"/\" menu, but this build has no handler for it", b.command)
		}
	}
	for _, tpl := range marketplaceTemplates {
		if tpl.Kind != slashModel.AppKindBuiltin {
			continue
		}
		for _, c := range tpl.Commands {
			if _, ok := commandRegistry[c.Command]; !ok {
				t.Errorf("the %s app offers /%s, but this build has no handler for it", tpl.Name, c.Command)
			}
		}
	}
}

// Words that make a command or an app an AI one.
var aboutAI = regexp.MustCompile(`\bAI\b|\bGPT\b`)

// The edition without AI holds nothing AI (the owner's rule for that line):
// an AI command or directory app is offered only by a build that has the AI
// package in it.
func TestNothingAIWithoutTheAIPackage(t *testing.T) {
	withAI := helpers.FeatureRegistered(helpers.FeatureNameAI)
	for _, b := range builtinCatalog {
		if !withAI && aboutAI.MatchString(b.description) {
			t.Errorf("/%s (%q) is an AI command in a build without AI", b.command, b.description)
		}
	}
	for _, tpl := range marketplaceTemplates {
		if !withAI && (tpl.Category == "AI" || aboutAI.MatchString(tpl.Name+" "+tpl.Description)) {
			t.Errorf("the %s app (%s) is an AI app in a build without AI", tpl.Name, tpl.Category)
		}
		for _, c := range tpl.Commands {
			if !withAI && aboutAI.MatchString(c.Description) {
				t.Errorf("the %s app's /%s (%q) is an AI command in a build without AI", tpl.Name, c.Command, c.Description)
			}
		}
	}
}
