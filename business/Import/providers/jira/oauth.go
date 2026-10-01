package jira

// Atlassian (Jira Cloud) OAuth 2.0 (3LO) endpoint config.
//
// The token endpoint is the only endpoint we actually need at runtime:
// we never initiate the authorize redirect from this provider (that's
// the connect endpoint upstream), but the oauth2.Config wants both URLs
// to be valid so we plug the canonical pair in.

import (
	"os"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"golang.org/x/oauth2"
)

// jiraOAuthEndpoint matches Atlassian's documented 3LO token URL.
// https://developer.atlassian.com/cloud/jira/platform/oauth-2-3lo-apps/
var jiraOAuthEndpoint = oauth2.Endpoint{
	AuthURL:  "https://auth.atlassian.com/authorize",
	TokenURL: "https://auth.atlassian.com/oauth/token",
}

// jiraOAuthConfig pulls credentials from env. Empty values are tolerated
// because PAT installs (the more common deployment) don't need them —
// FreshAccessToken returns the stored token unchanged in that case.
func jiraOAuthConfig() importProvider.OAuthConfig {
	return importProvider.OAuthConfig{
		ClientID:     os.Getenv("JIRA_OAUTH_CLIENT_ID"),
		ClientSecret: os.Getenv("JIRA_OAUTH_CLIENT_SECRET"),
		Endpoint:     jiraOAuthEndpoint,
	}
}
