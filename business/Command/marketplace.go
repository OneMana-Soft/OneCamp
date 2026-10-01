package business

// marketplace.go — a curated, one-click app directory (the "App Store" for the
// workspace). Each entry is a template that pre-fills everything tedious about
// installing an app: icon, description, the slash commands it provides, and —
// for OAuth apps — the provider's authorize/token URLs and scopes. Admins
// install with a single click; apps that need a credential (a Giphy API key, a
// Zoom client secret) are installed immediately and flagged "needs setup" so
// the admin finishes in the existing app editor — exactly like Slack's and
// Notion's directories (install ≠ configured).
//
// One-click uninstall removes the app, its commands, and its stored secrets
// (DeleteApp), and busts the catalog cache so the commands vanish everywhere.
//
// Design notes:
//   - Templates are the single source of truth here; installing just builds a
//     CreateAppRequest, so it flows through the same validated, encrypted path
//     as a manual install.
//   - Install is idempotent: installing an already-installed slug returns the
//     existing app rather than erroring or duplicating.
//   - "needs_setup" is computed by comparing each template's declared setup
//     requirements against the installed app's stored state, so the UI can
//     guide the admin to the one remaining step.

import (
	"context"
	"fmt"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	"github.com/akashc777/OneCamp/helpers"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
	"github.com/google/uuid"
)

// setupFieldType enumerates what an admin must supply to finish setup.
const (
	setupSecret    = "secret"      // an encrypted secret (api key)
	setupHandler   = "handler_url" // an external command handler endpoint
	setupOAuthCred = "oauth_cred"  // OAuth client id + secret
)

// appTemplate is one curated marketplace entry.
type appTemplate struct {
	Slug        string
	Name        string
	Description string
	Category    string
	IconURL     string
	Kind        string // external | oauth
	Featured    bool
	OAuthConfig *commandAdapter.AppOAuthConfig
	Commands    []commandAdapter.AppCommandInput
	Setup       []commandAdapter.SetupField
	SetupNote   string
}

// marketplaceTemplates is the curated directory. Giphy is fully functional
// in-process once a key is added; the OAuth/external entries pre-fill all the
// provider boilerplate so setup is just pasting credentials.
var marketplaceTemplates = []appTemplate{
	{
		Slug:        "giphy",
		Name:        "Giphy",
		Description: "Search and send GIFs right from the composer with /giphy.",
		Category:    "Fun",
		IconURL:     "https://cdn.simpleicons.org/giphy/00FF99",
		Kind:        slashModel.AppKindBuiltin,
		Featured:    true,
		Commands: []commandAdapter.AppCommandInput{
			{Command: "giphy", Description: "Search and send a GIF", UsageHint: "<search term>", ExecMode: slashModel.ExecInteractive, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "api_key", Label: "Giphy API key", Type: setupSecret, Required: true},
		},
		SetupNote: "Create a free API key at developers.giphy.com, then paste it here.",
	},
	{
		Slug:        "zoom",
		Name:        "Zoom",
		Description: "Start and share Zoom meetings with /zoom.",
		Category:    "Productivity",
		IconURL:     "https://cdn.simpleicons.org/zoom/0B5CFF",
		Kind:        slashModel.AppKindOAuth,
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			AuthURL:  "https://zoom.us/oauth/authorize",
			TokenURL: "https://zoom.us/oauth/token",
			Scopes:   []string{"meeting:write"},
		},
		Commands: []commandAdapter.AppCommandInput{
			{Command: "zoom", Description: "Start an instant Zoom meeting", UsageHint: "[topic]", ExecMode: slashModel.ExecExternal, ResponseType: "in_channel"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "oauth_cred", Label: "Zoom OAuth client ID & secret", Type: setupOAuthCred, Required: true},
		},
		SetupNote: "Create an OAuth app in the Zoom Marketplace and paste its client ID and secret, then click Connect.",
	},
	{
		Slug:        "jira",
		Name:        "Jira",
		Description: "Create and search Jira issues with /jira.",
		Category:    "Productivity",
		IconURL:     "https://cdn.simpleicons.org/jira/0052CC",
		Kind:        slashModel.AppKindOAuth,
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			AuthURL:  "https://auth.atlassian.com/authorize",
			TokenURL: "https://auth.atlassian.com/oauth/token",
			Scopes:   []string{"read:jira-work", "write:jira-work"},
		},
		Commands: []commandAdapter.AppCommandInput{
			{Command: "jira", Description: "Create or search a Jira issue", UsageHint: "create <summary>", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "oauth_cred", Label: "Atlassian OAuth client ID & secret", Type: setupOAuthCred, Required: true},
		},
		SetupNote: "Create an OAuth 2.0 (3LO) app in the Atlassian developer console, then paste its credentials and click Connect.",
	},
	{
		Slug:        "linear",
		Name:        "Linear",
		Description: "Create Linear issues without leaving chat with /linear.",
		Category:    "Productivity",
		IconURL:     "https://cdn.simpleicons.org/linear/5E6AD2",
		Kind:        slashModel.AppKindOAuth,
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			AuthURL:  "https://linear.app/oauth/authorize",
			TokenURL: "https://api.linear.app/oauth/token",
			Scopes:   []string{"write"},
		},
		Commands: []commandAdapter.AppCommandInput{
			{Command: "linear", Description: "Create a Linear issue", UsageHint: "<title>", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "oauth_cred", Label: "Linear OAuth client ID & secret", Type: setupOAuthCred, Required: true},
		},
		SetupNote: "Create an OAuth application in Linear settings, then paste its credentials and click Connect.",
	},
	{
		Slug:        "asana",
		Name:        "Asana",
		Description: "Add Asana tasks from the composer with /asana.",
		Category:    "Productivity",
		IconURL:     "https://cdn.simpleicons.org/asana/F06A6A",
		Kind:        slashModel.AppKindOAuth,
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			AuthURL:  "https://app.asana.com/-/oauth_authorize",
			TokenURL: "https://app.asana.com/-/oauth_token",
			Scopes:   []string{"default"},
		},
		Commands: []commandAdapter.AppCommandInput{
			{Command: "asana", Description: "Create an Asana task", UsageHint: "<task name>", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "oauth_cred", Label: "Asana OAuth client ID & secret", Type: setupOAuthCred, Required: true},
		},
		SetupNote: "Register an app in the Asana developer console, then paste its credentials and click Connect.",
	},
	{
		Slug:        "pagerduty",
		Name:        "PagerDuty",
		Description: "Trigger and ack incidents with /pd.",
		Category:    "DevOps",
		IconURL:     "https://cdn.simpleicons.org/pagerduty/06AC38",
		Kind:        slashModel.AppKindOAuth,
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			AuthURL:  "https://identity.pagerduty.com/global/oauth/authorize",
			TokenURL: "https://identity.pagerduty.com/global/oauth/token",
			Scopes:   []string{"incidents.write"},
		},
		Commands: []commandAdapter.AppCommandInput{
			{Command: "pd", Description: "Trigger a PagerDuty incident", UsageHint: "<title>", ExecMode: slashModel.ExecExternal, ResponseType: "in_channel"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "oauth_cred", Label: "PagerDuty OAuth client ID & secret", Type: setupOAuthCred, Required: true},
		},
		SetupNote: "Create an OAuth app in PagerDuty, then paste its credentials and click Connect.",
	},
	{
		Slug:        "trello",
		Name:        "Trello",
		Description: "Add Trello cards from chat with /trello.",
		Category:    "Productivity",
		IconURL:     "https://cdn.simpleicons.org/trello/0052CC",
		Kind:        slashModel.AppKindExternal,
		Commands: []commandAdapter.AppCommandInput{
			{Command: "trello", Description: "Add a card to a Trello board", UsageHint: "<card title>", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "api_key", Label: "Trello API key", Type: setupSecret, Required: true},
		},
		SetupNote: "Get an API key from trello.com/app-key and paste it here.",
	},
	{
		Slug:        "notion",
		Name:        "Notion",
		Description: "Search your Notion workspace and create pages with /notion.",
		Category:    "Productivity",
		IconURL:     "https://cdn.simpleicons.org/notion/000000",
		Kind:        slashModel.AppKindOAuth,
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			AuthURL:  "https://api.notion.com/v1/oauth/authorize",
			TokenURL: "https://api.notion.com/v1/oauth/token",
			Scopes:   []string{},
		},
		Commands: []commandAdapter.AppCommandInput{
			{Command: "notion", Description: "Search Notion or create a page", UsageHint: "<query>", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "oauth_cred", Label: "Notion OAuth client ID & secret", Type: setupOAuthCred, Required: true},
		},
		SetupNote: "Create a public integration at notion.so/my-integrations, then paste its OAuth client ID and secret and click Connect.",
	},
	{
		Slug:        "todoist",
		Name:        "Todoist",
		Description: "Capture tasks into Todoist without leaving chat with /todoist.",
		Category:    "Productivity",
		IconURL:     "https://cdn.simpleicons.org/todoist/E44332",
		Kind:        slashModel.AppKindOAuth,
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			AuthURL:  "https://todoist.com/oauth/authorize",
			TokenURL: "https://todoist.com/oauth/access_token",
			Scopes:   []string{"data:read_write"},
		},
		Commands: []commandAdapter.AppCommandInput{
			{Command: "todoist", Description: "Add a task to Todoist", UsageHint: "<task>", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "oauth_cred", Label: "Todoist OAuth client ID & secret", Type: setupOAuthCred, Required: true},
		},
		SetupNote: "Create an app in the Todoist App Console, then paste its client ID and secret and click Connect.",
	},
	{
		Slug:        "clickup",
		Name:        "ClickUp",
		Description: "Create ClickUp tasks from the composer with /clickup.",
		Category:    "Productivity",
		IconURL:     "https://cdn.simpleicons.org/clickup/7B68EE",
		Kind:        slashModel.AppKindOAuth,
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			AuthURL:  "https://app.clickup.com/api",
			TokenURL: "https://api.clickup.com/api/v2/oauth/token",
			Scopes:   []string{},
		},
		Commands: []commandAdapter.AppCommandInput{
			{Command: "clickup", Description: "Create a ClickUp task", UsageHint: "<task name>", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "oauth_cred", Label: "ClickUp OAuth client ID & secret", Type: setupOAuthCred, Required: true},
		},
		SetupNote: "Create an OAuth app in ClickUp settings, then paste its client ID and secret and click Connect.",
	},
	{
		Slug:        "google_drive",
		Name:        "Google Drive",
		Description: "Search and share Google Drive files with /drive.",
		Category:    "Productivity",
		IconURL:     "https://cdn.simpleicons.org/googledrive/4285F4",
		Kind:        slashModel.AppKindOAuth,
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
			TokenURL: "https://oauth2.googleapis.com/token",
			Scopes:   []string{"https://www.googleapis.com/auth/drive.readonly"},
		},
		Commands: []commandAdapter.AppCommandInput{
			{Command: "drive", Description: "Search Google Drive files", UsageHint: "<query>", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "oauth_cred", Label: "Google OAuth client ID & secret", Type: setupOAuthCred, Required: true},
		},
		SetupNote: "Create OAuth credentials in Google Cloud Console with the Drive scope, then paste them and click Connect.",
	},
	{
		Slug:        "gitlab",
		Name:        "GitLab",
		Description: "Check merge requests and issues with /gitlab.",
		Category:    "Developer",
		IconURL:     "https://cdn.simpleicons.org/gitlab/FC6D26",
		Kind:        slashModel.AppKindOAuth,
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			AuthURL:  "https://gitlab.com/oauth/authorize",
			TokenURL: "https://gitlab.com/oauth/token",
			Scopes:   []string{"read_api"},
		},
		Commands: []commandAdapter.AppCommandInput{
			{Command: "gitlab", Description: "Look up GitLab MRs/issues", UsageHint: "mrs | issues", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "oauth_cred", Label: "GitLab application ID & secret", Type: setupOAuthCred, Required: true},
		},
		SetupNote: "Create an OAuth application in GitLab (User Settings → Applications), then paste its ID and secret and click Connect.",
	},
	{
		Slug:        "sentry",
		Name:        "Sentry",
		Description: "Surface and triage Sentry issues with /sentry.",
		Category:    "Developer",
		IconURL:     "https://cdn.simpleicons.org/sentry/362D59",
		Kind:        slashModel.AppKindExternal,
		Commands: []commandAdapter.AppCommandInput{
			{Command: "sentry", Description: "List recent Sentry issues", UsageHint: "[project]", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "api_key", Label: "Sentry auth token", Type: setupSecret, Required: true},
		},
		SetupNote: "Create an internal integration / auth token in Sentry settings and paste it here.",
	},
	{
		Slug:        "datadog",
		Name:        "Datadog",
		Description: "Query monitors and metrics with /datadog.",
		Category:    "Developer",
		IconURL:     "https://cdn.simpleicons.org/datadog/632CA6",
		Kind:        slashModel.AppKindExternal,
		Commands: []commandAdapter.AppCommandInput{
			{Command: "datadog", Description: "Check Datadog monitors", UsageHint: "monitors", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "api_key", Label: "Datadog API key", Type: setupSecret, Required: true},
		},
		SetupNote: "Create an API key in Datadog (Organization Settings → API Keys) and paste it here.",
	},
	{
		Slug:        "opsgenie",
		Name:        "Opsgenie",
		Description: "Create and ack alerts with /opsgenie.",
		Category:    "DevOps",
		IconURL:     "https://cdn.simpleicons.org/opsgenie/172B4D",
		Kind:        slashModel.AppKindExternal,
		Commands: []commandAdapter.AppCommandInput{
			{Command: "opsgenie", Description: "Create an Opsgenie alert", UsageHint: "<message>", ExecMode: slashModel.ExecExternal, ResponseType: "in_channel"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "api_key", Label: "Opsgenie API key", Type: setupSecret, Required: true},
		},
		SetupNote: "Create an API integration in Opsgenie and paste its API key here.",
	},
	{
		Slug:        "calendly",
		Name:        "Calendly",
		Description: "Share your scheduling link instantly with /calendly.",
		Category:    "Meetings",
		IconURL:     "https://cdn.simpleicons.org/calendly/006BFF",
		Kind:        slashModel.AppKindOAuth,
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			AuthURL:  "https://auth.calendly.com/oauth/authorize",
			TokenURL: "https://auth.calendly.com/oauth/token",
			Scopes:   []string{"default"},
		},
		Commands: []commandAdapter.AppCommandInput{
			{Command: "calendly", Description: "Share your Calendly link", UsageHint: "", ExecMode: slashModel.ExecExternal, ResponseType: "in_channel"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "oauth_cred", Label: "Calendly OAuth client ID & secret", Type: setupOAuthCred, Required: true},
		},
		SetupNote: "Create an OAuth app in the Calendly developer portal, then paste its credentials and click Connect.",
	},
	{
		Slug:        "hubspot",
		Name:        "HubSpot",
		Description: "Look up CRM contacts and deals with /hubspot.",
		Category:    "Sales & Support",
		IconURL:     "https://cdn.simpleicons.org/hubspot/FF7A59",
		Kind:        slashModel.AppKindOAuth,
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			AuthURL:  "https://app.hubspot.com/oauth/authorize",
			TokenURL: "https://api.hubapi.com/oauth/v1/token",
			Scopes:   []string{"crm.objects.contacts.read"},
		},
		Commands: []commandAdapter.AppCommandInput{
			{Command: "hubspot", Description: "Search HubSpot contacts/deals", UsageHint: "<query>", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "oauth_cred", Label: "HubSpot OAuth client ID & secret", Type: setupOAuthCred, Required: true},
		},
		SetupNote: "Create an app in the HubSpot developer account, then paste its client ID and secret and click Connect.",
	},
	{
		Slug:        "zendesk",
		Name:        "Zendesk",
		Description: "Create and search support tickets with /zendesk.",
		Category:    "Sales & Support",
		IconURL:     "https://cdn.simpleicons.org/zendesk/03363D",
		Kind:        slashModel.AppKindExternal,
		Commands: []commandAdapter.AppCommandInput{
			{Command: "zendesk", Description: "Create or search a Zendesk ticket", UsageHint: "<subject>", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "api_key", Label: "Zendesk API token", Type: setupSecret, Required: true},
		},
		SetupNote: "Create an API token in Zendesk Admin → Apps and integrations → APIs, then paste it here.",
	},
	{
		Slug:        "stripe",
		Name:        "Stripe",
		Description: "Get payment and customer info with /stripe.",
		Category:    "Sales & Support",
		IconURL:     "https://cdn.simpleicons.org/stripe/635BFF",
		Kind:        slashModel.AppKindExternal,
		Commands: []commandAdapter.AppCommandInput{
			{Command: "stripe", Description: "Look up a Stripe customer or payment", UsageHint: "<email|id>", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "api_key", Label: "Stripe restricted API key", Type: setupSecret, Required: true},
		},
		SetupNote: "Create a restricted (read-only) API key in the Stripe dashboard and paste it here.",
	},
	{
		Slug:        "figma",
		Name:        "Figma",
		Description: "Unfurl and share Figma files with /figma.",
		Category:    "Design",
		IconURL:     "https://cdn.simpleicons.org/figma/F24E1E",
		Kind:        slashModel.AppKindOAuth,
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			AuthURL:  "https://www.figma.com/oauth",
			TokenURL: "https://api.figma.com/v1/oauth/token",
			Scopes:   []string{"file_read"},
		},
		Commands: []commandAdapter.AppCommandInput{
			{Command: "figma", Description: "Search and share Figma files", UsageHint: "<query>", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "oauth_cred", Label: "Figma OAuth client ID & secret", Type: setupOAuthCred, Required: true},
		},
		SetupNote: "Create an app at figma.com/developers/apps, then paste its client ID and secret and click Connect.",
	},
	{
		Slug:        "openai",
		Name:        "OpenAI",
		Description: "Ask GPT a quick question with /gpt.",
		Category:    "AI",
		IconURL:     "https://www.google.com/s2/favicons?domain=openai.com&sz=128",
		Kind:        slashModel.AppKindExternal,
		Commands: []commandAdapter.AppCommandInput{
			{Command: "gpt", Description: "Ask OpenAI GPT a quick question", UsageHint: "<prompt>", ExecMode: slashModel.ExecExternal, ResponseType: "ephemeral"},
		},
		Setup: []commandAdapter.SetupField{
			{Key: "api_key", Label: "OpenAI API key", Type: setupSecret, Required: true},
		},
		SetupNote: "Create a key at platform.openai.com/api-keys and paste it here.",
	},
}

// MarketplaceTemplates returns the curated catalog (no per-workspace state).
func MarketplaceTemplates() []appTemplate {
	return marketplaceTemplates
}

func getTemplate(slug string) (appTemplate, bool) {
	for _, t := range marketplaceTemplates {
		if t.Slug == slug {
			return t, true
		}
	}
	return appTemplate{}, false
}

// ListMarketplace returns the curated catalog enriched with this workspace's
// install state (installed / enabled / needs-setup / connected) so the UI can
// render the right action for each card. Installed apps are fetched in ONE
// query and indexed by slug, so the whole directory costs a single apps read
// plus a secret-bag read only for the apps that are actually installed.
func ListMarketplace(ctx context.Context) ([]commandAdapter.MarketplaceItem, error) {
	installed, _ := slashModel.ListApps(ctx)
	bySlug := make(map[string]*slashModel.App, len(installed))
	for _, a := range installed {
		bySlug[a.Slug] = a
	}

	out := make([]commandAdapter.MarketplaceItem, 0, len(marketplaceTemplates))
	for _, t := range marketplaceTemplates {
		item := commandAdapter.MarketplaceItem{
			Slug:        t.Slug,
			Name:        t.Name,
			Description: t.Description,
			Category:    t.Category,
			IconURL:     t.IconURL,
			Kind:        t.Kind,
			Featured:    t.Featured,
			SetupNote:   t.SetupNote,
			Setup:       t.Setup,
			Commands:    make([]string, 0, len(t.Commands)),
		}
		for _, c := range t.Commands {
			item.Commands = append(item.Commands, c.Command)
		}

		if app := bySlug[t.Slug]; app != nil {
			item.Installed = true
			item.Enabled = app.IsEnabled
			item.AppID = app.Id.String()
			// Load the secret bag once and reuse it for both the OAuth-connected
			// check and the needs-setup check, instead of decrypting repeatedly.
			bag, _ := loadAppSecrets(ctx, app.Id)
			if t.Kind == slashModel.AppKindOAuth {
				item.IsConnected = appOAuthConnected(ctx, app.Id)
			}
			item.NeedsSetup = templateNeedsSetup(t, app, bag)
		}
		out = append(out, item)
	}
	return out, nil
}

// templateNeedsSetup reports whether an installed app is still missing a
// required credential/endpoint declared by its template. The decrypted secret
// bag is passed in (loaded once by the caller) so this is allocation-free.
func templateNeedsSetup(t appTemplate, app *slashModel.App, bag *appSecretBag) bool {
	for _, f := range t.Setup {
		if !f.Required {
			continue
		}
		switch f.Type {
		case setupSecret:
			if bag == nil || bag.Secrets[f.Key] == "" {
				return true
			}
		case setupHandler:
			if app.HandlerUrl == nil || *app.HandlerUrl == "" {
				return true
			}
		case setupOAuthCred:
			if bag == nil || bag.OAuthClientSecret == "" {
				return true
			}
		}
	}
	return false
}

// InstallTemplate one-click-installs a marketplace template. Idempotent:
// re-installing an existing slug returns the current app instead of
// duplicating. The created app inherits the template's commands and (for OAuth
// apps) the provider URLs/scopes; secrets are added later in the editor.
func InstallTemplate(ctx context.Context, slug string, installedBy uuid.UUID) (*commandAdapter.AppView, error) {
	t, ok := getTemplate(slug)
	if !ok {
		return nil, fmt.Errorf("unknown app: %s", slug)
	}

	// Idempotency: if already installed (and not soft-deleted), return it.
	// Self-heal drift: an earlier version installed Giphy as "external"; if the
	// stored kind no longer matches the template (now "builtin"), correct it and
	// remove any stale app-linked command rows so the editor stops showing an
	// empty Handler URL / "No commands yet" for a working built-in.
	if existing, _ := slashModel.GetAppBySlug(ctx, t.Slug); existing != nil {
		if existing.Kind != t.Kind {
			_ = slashModel.UpdateApp(ctx, existing.Id, map[string]interface{}{"kind": t.Kind})
			if t.Kind == slashModel.AppKindBuiltin {
				_ = slashModel.DeleteCommandsByApp(ctx, existing.Id)
			}
			bumpCatalogVersion(ctx)
			return buildAppView(ctx, existing.Id)
		}
		return buildAppViewFrom(ctx, existing)
	}

	req := commandAdapter.CreateAppRequest{
		Slug:        t.Slug,
		Name:        t.Name,
		Description: t.Description,
		IconURL:     t.IconURL,
		Kind:        t.Kind,
		OAuthConfig: t.OAuthConfig,
		Commands:    t.Commands,
	}
	return CreateApp(ctx, req, installedBy)
}

// UninstallTemplate one-click-uninstalls a marketplace app by slug: removes the
// app, its commands, and its encrypted secrets, and busts the catalog cache.
// Idempotent: uninstalling something not installed is a no-op success.
func UninstallTemplate(ctx context.Context, slug string) error {
	app, err := slashModel.GetAppBySlug(ctx, slug)
	if err != nil {
		return err
	}
	if app == nil {
		return nil // already gone
	}
	return DeleteApp(ctx, app.Id)
}

// ReconcileInstalledApps corrects drift between installed apps and the current
// marketplace templates at startup. Today it fixes the app "kind" (an earlier
// build installed Giphy as "external"; it is now "builtin"), removing any stale
// app-linked command rows for apps that became built-in. Idempotent and
// best-effort: a failure on one app is logged and never blocks boot.
func ReconcileInstalledApps(ctx context.Context) {
	changed := false
	for _, t := range marketplaceTemplates {
		app, err := slashModel.GetAppBySlug(ctx, t.Slug)
		if err != nil || app == nil {
			continue
		}
		if app.Kind == t.Kind {
			continue
		}
		if err := slashModel.UpdateApp(ctx, app.Id, map[string]interface{}{"kind": t.Kind}); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Command/ReconcileInstalledApps update %s err: %+v", t.Slug, err)
			continue
		}
		if t.Kind == slashModel.AppKindBuiltin {
			_ = slashModel.DeleteCommandsByApp(ctx, app.Id)
		}
		changed = true
		helpers.MessageLogs.InfoLog.Printf("Reconciled app %s kind → %s", t.Slug, t.Kind)
	}
	if changed {
		bumpCatalogVersion(ctx)
	}
}
