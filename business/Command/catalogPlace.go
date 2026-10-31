package business

import slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"

// menuPlace is where a command stands in a person's "/" menu.
type menuPlace int

const (
	menuShown  menuPlace = iota // offered as it is
	menuSetup                   // its app isn't set up: admins are offered to set it up, members don't see it
	menuHidden                  // its app is turned off or was removed: nobody sees it
)

// latestApp is a slug's app as the "/" menu weighs it: the installed one, or
// none because an admin removed it (Removed, App nil), so an app taken away on
// purpose is told apart from one never installed.
type latestApp struct {
	*slashModel.App
	Removed bool
}

// wired reports whether this build can run a command, as Execute would: an
// app's command goes out to its app, and any other needs a registered
// handler. A seeded built-in without one (without-ai seeds /ask and wires no
// handler for it) would only answer that it's unavailable.
func wired(c *slashModel.SlashCommand) bool {
	if c.ExecMode == slashModel.ExecExternal {
		return true
	}
	_, ok := commandRegistry[normalizeCommand(c.Command)]
	return ok
}

// commandTemplate is the directory template a command comes from: its app's,
// or for a seeded built-in, the built-in template that lists it (Giphy's
// /giphy). A custom app's command and a built-in no app stands behind (/remind)
// have none.
func commandTemplate(c *slashModel.SlashCommand) (appTemplate, bool) {
	if c.AppSlug != nil {
		return getTemplate(*c.AppSlug)
	}
	if !c.IsBuiltin {
		return appTemplate{}, false
	}
	for _, t := range marketplaceTemplates {
		if t.Kind != slashModel.AppKindBuiltin {
			continue
		}
		for _, tc := range t.Commands {
			if tc.Command == c.Command {
				return t, true
			}
		}
	}
	return appTemplate{}, false
}

// commandPlace decides a command's place from its template (ok false: none)
// and its app (nil: never installed; bag: the app's secrets). An app is set
// up once its template's required fields are filled, so a never-installed app
// with none, such as OneCamp AI's, is offered as it is.
func commandPlace(t appTemplate, ok bool, app *latestApp, bag *appSecretBag) menuPlace {
	switch {
	case !ok:
		return menuShown
	case app == nil:
		if templateNeedsSetup(t, &slashModel.App{}, nil) {
			return menuSetup
		}
		return menuShown
	case app.Removed || !app.IsEnabled:
		return menuHidden
	case templateNeedsSetup(t, app.App, bag):
		return menuSetup
	}
	return menuShown
}
