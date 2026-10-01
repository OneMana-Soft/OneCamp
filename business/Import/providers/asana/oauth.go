package asana

// Asana OAuth 2.0 endpoint config.
//
// Token endpoint: https://app.asana.com/-/oauth_token (POST with
// grant_type=refresh_token returns a new access_token + 1h expiry).
// https://developers.asana.com/docs/oauth

import (
	"os"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"golang.org/x/oauth2"
)

var asanaOAuthEndpoint = oauth2.Endpoint{
	AuthURL:  "https://app.asana.com/-/oauth_authorize",
	TokenURL: "https://app.asana.com/-/oauth_token",
}

// asanaOAuthConfig pulls credentials from env. Empty values are
// tolerated because PAT installs don't need them — FreshAccessToken
// returns the stored token unchanged in that case.
func asanaOAuthConfig() importProvider.OAuthConfig {
	return importProvider.OAuthConfig{
		ClientID:     os.Getenv("ASANA_OAUTH_CLIENT_ID"),
		ClientSecret: os.Getenv("ASANA_OAUTH_CLIENT_SECRET"),
		Endpoint:     asanaOAuthEndpoint,
	}
}
