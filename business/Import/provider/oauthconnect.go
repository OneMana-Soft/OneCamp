package provider

// Connecting by signing in at the provider, for a provider with no personal
// tokens to paste (Basecamp: OAuth 2 through Launchpad only; its refresh is
// OAuth 2's own, so FreshAccessToken keeps the token fresh). The admin is
// sent to the provider's authorize page and comes back to the server's
// callback (business/Import.FinishOAuthConnect), which saves the token as a
// pasted one is saved. A provider opts in with OAuthConnector; it is offered
// only on a server with the provider's client id and secret.

import (
	"context"

	"golang.org/x/oauth2"
)

// OAuthConnector is a provider an admin connects by signing in.
type OAuthConnector interface {
	OAuthConnect() OAuthConnect
}

// OAuthConnect is how a provider's sign-in goes.
type OAuthConnect struct {
	Config OAuthConfig
	Scopes []string
	// Setup says how a server is given the provider's app, for the admin
	// of a server that has none.
	Setup string
}

// Configured is a server with the provider's client id and secret.
func (c OAuthConnect) Configured() bool {
	return c.Config.ClientID != "" && c.Config.ClientSecret != ""
}

func (c OAuthConnect) config(redirectURI string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.Config.ClientID,
		ClientSecret: c.Config.ClientSecret,
		Endpoint:     c.Config.Endpoint,
		RedirectURL:  redirectURI,
		Scopes:       c.Scopes,
	}
}

// AuthURL is the provider's authorize page for one sign-in.
func (c OAuthConnect) AuthURL(state, redirectURI string) string {
	return c.config(redirectURI).AuthCodeURL(state)
}

// Exchange trades the code the provider sent back for a token.
func (c OAuthConnect) Exchange(ctx context.Context, code, redirectURI string) (*oauth2.Token, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, SharedAPIClient)
	return c.config(redirectURI).Exchange(ctx, code)
}
