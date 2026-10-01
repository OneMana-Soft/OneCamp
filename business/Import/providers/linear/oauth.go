package linear

// Linear OAuth 2.0 endpoint config.
//
// Linear's API docs:
//   Authorize: https://linear.app/oauth/authorize
//   Token:     https://api.linear.app/oauth/token
//
// Linear access tokens issued via the OAuth flow are long-lived (no
// refresh token in the standard grant), so the refresh path is a
// no-op — but we wire the helper anyway so a future short-lived
// scope (Linear may add one) Just Works without code changes.

import (
	"os"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"golang.org/x/oauth2"
)

var linearOAuthEndpoint = oauth2.Endpoint{
	AuthURL:  "https://linear.app/oauth/authorize",
	TokenURL: "https://api.linear.app/oauth/token",
}

// linearOAuthConfig pulls credentials from env. PAT installs (the more
// common path) leave both blank — FreshAccessToken returns the stored
// access token unchanged in that case.
func linearOAuthConfig() importProvider.OAuthConfig {
	return importProvider.OAuthConfig{
		ClientID:     os.Getenv("LINEAR_OAUTH_CLIENT_ID"),
		ClientSecret: os.Getenv("LINEAR_OAUTH_CLIENT_SECRET"),
		Endpoint:     linearOAuthEndpoint,
	}
}
