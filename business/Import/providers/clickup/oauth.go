package clickup

// ClickUp OAuth 2.0 endpoint config.
//
// Docs: https://developer.clickup.com/docs/authentication
//   Authorize: https://app.clickup.com/api
//   Token:     https://api.clickup.com/api/v2/oauth/token
//
// ClickUp OAuth tokens currently don't expire, so the refresh path is
// effectively a no-op for typical installs. We still wire the helper
// in so a future short-lived scope (or PAT rotation) Just Works.

import (
	"os"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"golang.org/x/oauth2"
)

var clickupOAuthEndpoint = oauth2.Endpoint{
	AuthURL:  "https://app.clickup.com/api",
	TokenURL: "https://api.clickup.com/api/v2/oauth/token",
}

// clickupOAuthConfig pulls credentials from env. PAT installs (the
// more common path: a single API token from
// https://app.clickup.com/<id>/settings/apps) leave both blank —
// FreshAccessToken returns the stored access unchanged in that case.
func clickupOAuthConfig() importProvider.OAuthConfig {
	return importProvider.OAuthConfig{
		ClientID:     os.Getenv("CLICKUP_OAUTH_CLIENT_ID"),
		ClientSecret: os.Getenv("CLICKUP_OAUTH_CLIENT_SECRET"),
		Endpoint:     clickupOAuthEndpoint,
	}
}
