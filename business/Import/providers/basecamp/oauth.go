package basecamp

// Basecamp has no personal tokens: an admin connects by signing in through
// Launchpad, 37signals' OAuth 2 service, with this server's app (registered
// at launchpad.37signals.com/integrations, its redirect URI the server's
// /admin/import/oauth/callback). Access tokens last two weeks; the refresh
// is OAuth 2's own, so importProvider.FreshAccessToken keeps them fresh.

import (
	"os"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"golang.org/x/oauth2"
)

func oauthConfig() importProvider.OAuthConfig {
	return importProvider.OAuthConfig{
		ClientID:     os.Getenv("BASECAMP_OAUTH_CLIENT_ID"),
		ClientSecret: os.Getenv("BASECAMP_OAUTH_CLIENT_SECRET"),
		Endpoint: oauth2.Endpoint{
			AuthURL:  launchpadBase + "/authorization/new",
			TokenURL: launchpadBase + "/authorization/token",
		},
	}
}

// OAuthConnect is how an admin signs in to Basecamp.
func (p *Provider) OAuthConnect() importProvider.OAuthConnect {
	return importProvider.OAuthConnect{
		Config: oauthConfig(),
		Setup: "Signing in to Basecamp isn't set up on this server. Register an app at launchpad.37signals.com/integrations " +
			"with the redirect URI https://<this server's API address>/admin/import/oauth/callback, then set BASECAMP_OAUTH_CLIENT_ID and BASECAMP_OAUTH_CLIENT_SECRET.",
	}
}
