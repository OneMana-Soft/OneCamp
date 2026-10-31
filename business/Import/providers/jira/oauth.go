package jira

// Atlassian (Jira Cloud) OAuth 2.0 (3LO) endpoint config.
//
// The token endpoint is the only endpoint we actually need at runtime:
// we never initiate the authorize redirect from this provider (that's
// the connect endpoint upstream), but the oauth2.Config wants both URLs
// to be valid so we plug the canonical pair in.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers"
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

// OAuthConnect is how an admin signs in to Atlassian (3LO), beside pasting an
// API token.
func (p *Provider) OAuthConnect() importProvider.OAuthConnect {
	return importProvider.OAuthConnect{
		Config:     jiraOAuthConfig(),
		Scopes:     []string{"read:jira-work", "read:jira-user", "offline_access"},
		AuthParams: map[string]string{"audience": "api.atlassian.com", "prompt": "consent"},
		Optional:   true,
		Setup: "Signing in to Atlassian isn't set up on this server. Create an OAuth 2.0 (3LO) app at developer.atlassian.com/console/myapps " +
			"with the callback URL https://<this server's API address>/admin/import/oauth/callback and the Jira scopes read:jira-work and read:jira-user, " +
			"then set JIRA_OAUTH_CLIENT_ID and JIRA_OAUTH_CLIENT_SECRET. An API token works without it.",
	}
}

// CompleteOAuth finds the Jira site a signed-in token is for, among those it
// reaches (Atlassian's accessible-resources): the one at the address the
// admin named, or the only one. It never guesses between several.
func (p *Provider) CompleteOAuth(ctx context.Context, accessToken, site string) (json.RawMessage, error) {
	var sites []struct {
		ID   string `json:"id"`
		URL  string `json:"url"`
		Name string `json:"name"`
	}
	if err := p.getJSON(ctx, "Bearer "+accessToken, atlassianAPI+"/oauth/token/accessible-resources", &sites); err != nil {
		return nil, errors.New("Atlassian didn't say which Jira sites this sign-in reaches. Connect again.")
	}
	// An address typed without its scheme ("acme.atlassian.net") is the
	// https one Atlassian lists.
	if site = strings.TrimSpace(site); site != "" && !strings.Contains(site, "://") {
		site = "https://" + site
	}
	var picked []int
	for i, s := range sites {
		if s.ID != "" && (site == "" || helpers.SameOrigin(s.URL, site)) {
			picked = append(picked, i)
		}
	}
	switch {
	case len(picked) == 1:
		s := sites[picked[0]]
		return json.Marshal(tokenMeta{SiteURL: strings.TrimRight(s.URL, "/"), CloudID: s.ID, Bearer: "true"})
	case len(sites) == 0:
		return nil, errors.New("This Atlassian sign-in reaches no Jira site. Sign in with an account that can see Jira.")
	case site != "":
		return nil, errors.New("This Atlassian sign-in can't reach " + site + ". Check the address, or sign in with an account on that site.")
	}
	names := make([]string, 0, len(sites))
	for _, s := range sites {
		names = append(names, s.URL)
	}
	return nil, errors.New("This Atlassian sign-in reaches several Jira sites (" + strings.Join(names, ", ") + "). Connect again with the address of the one to import.")
}

// atlassianAPI is where a signed-in (3LO) token is used; tests point it at a
// fake.
var atlassianAPI = "https://api.atlassian.com"

// tokenMeta is what a Jira connection keeps beside its token.
type tokenMeta struct {
	SiteURL string `json:"site_url"`
	CloudID string `json:"cloud_id"`
	Email   string `json:"email"`
	Bearer  string `json:"bearer"`
}

// bases is where a connection's API calls go, and where its issues are
// browsed. A signed-in (3LO, bearer) token reaches Jira only through
// Atlassian's gateway under the site's cloud id; the site's own address
// answers it 401 and serves only as the links' address. An API token works
// at the site. Pure.
func (m tokenMeta) bases() (api, browse string, err error) {
	site := strings.TrimRight(m.SiteURL, "/")
	if m.Bearer == "true" && m.CloudID != "" {
		return atlassianAPI + "/ex/jira/" + m.CloudID, site, nil
	}
	if site == "" {
		return "", "", errors.New("jira token missing metadata.site_url; reconnect with the Atlassian site URL")
	}
	return site, site, nil
}
