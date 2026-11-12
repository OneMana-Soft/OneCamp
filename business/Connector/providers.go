package business

import "github.com/akashc777/OneCamp/initializers/oauth"

// providers.go — the connector catalog. Each provider reuses an admin-managed
// OAuth client ("google" or "github") and declares the least-privilege scope
// set it needs, with human-readable permission descriptions for the consent
// screen. Read scopes are preferred; write scopes are only requested where a
// write tool exists, and those tools always run behind the AI confirmation gate.

func init() {
	// Gmail — read + send on the user's behalf.
	register(Provider{
		ID:          ProviderGmail,
		Name:        "Gmail",
		Description: "Let the AI search and summarise your email, and draft replies for your approval.",
		IconKey:     "gmail",
		reuseClient: "google",
		AuthURL:     oauth.GoogleEndpoint.AuthURL,
		TokenURL:    oauth.GoogleEndpoint.TokenURL,
		RevokeURL:   "https://oauth2.googleapis.com/revoke",
		Scopes: []string{
			"https://www.googleapis.com/auth/gmail.readonly",
			"https://www.googleapis.com/auth/gmail.send",
		},
		Permissions: []ScopeInfo{
			{Capability: CapabilityRead, Description: "Read and search your email messages"},
			{Capability: CapabilityWrite, Description: "Send emails on your behalf (only after you confirm)"},
		},
	})

	// Google Calendar — reuses the existing calendar integration row/provider.
	register(Provider{
		ID:          ProviderCalendar,
		Name:        "Google Calendar",
		Description: "Let the AI see your schedule, find free time, and create events for your approval.",
		IconKey:     "calendar",
		reuseClient: "google",
		AuthURL:     oauth.GoogleEndpoint.AuthURL,
		TokenURL:    oauth.GoogleEndpoint.TokenURL,
		RevokeURL:   "https://oauth2.googleapis.com/revoke",
		Scopes: []string{
			"https://www.googleapis.com/auth/calendar.events",
			"https://www.googleapis.com/auth/calendar.readonly",
		},
		Permissions: []ScopeInfo{
			{Capability: CapabilityRead, Description: "View your calendars and events"},
			{Capability: CapabilityWrite, Description: "Create and update events (only after you confirm)"},
		},
	})

	// Outlook Calendar (calendar B7) — two-way calendar sync through
	// Microsoft Graph, on the admin's own free Microsoft (Entra) app. The
	// "common" endpoint signs in work accounts and Outlook.com alike. Offered
	// only once that app is set (hideUnconfigured).
	register(Provider{
		ID:          ProviderOutlookCalendar,
		Name:        "Outlook Calendar",
		Description: "Move your Outlook calendars into OneCamp and keep both in step.",
		IconKey:     "outlook",
		reuseClient: "microsoft",
		AuthURL:     oauth.MicrosoftEndpoint.AuthURL,
		TokenURL:    oauth.MicrosoftEndpoint.TokenURL,
		// Microsoft has no endpoint to revoke one token: disconnecting
		// deletes it (people can also remove the app from their account).
		RevokeURL: "",
		Scopes:    []string{"offline_access", "User.Read", "Calendars.ReadWrite"},
		Permissions: []ScopeInfo{
			{Capability: CapabilityRead, Description: "Read your calendars and events"},
			{Capability: CapabilityWrite, Description: "Create, change and delete the events you change in OneCamp"},
		},
		hideUnconfigured: true,
	})

	// GitHub — per-user PRs, issues, repos. Distinct from the GitHub App.
	register(Provider{
		ID:          ProviderGitHub,
		Name:        "GitHub",
		Description: "Let the AI check your pull requests, issues, and notifications, and comment for your approval.",
		IconKey:     "github",
		reuseClient: "github",
		AuthURL:     "https://github.com/login/oauth/authorize",
		TokenURL:    "https://github.com/login/oauth/access_token",
		// GitHub has no standard token revoke endpoint usable with just the
		// token; disconnect deletes the stored token (users can also revoke in
		// GitHub settings). Left empty intentionally.
		RevokeURL: "",
		Scopes:    []string{"repo", "read:user", "read:org", "notifications"},
		Permissions: []ScopeInfo{
			{Capability: CapabilityRead, Description: "Read your repositories, pull requests, issues, and notifications"},
			{Capability: CapabilityWrite, Description: "Comment on issues and pull requests (only after you confirm)"},
		},
	})
}
