package oauth

import (
	"context"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	githubOAuth2 "golang.org/x/oauth2/github"

	"os"
)

type OAuthProvider struct {
	GoogleConfig         *oauth2.Config
	GithubConfig         *oauth2.Config
	GoogleCalendarConfig *oauth2.Config
}

// OIDCProviders is a struct that contains reference all the OpenID providers
type OIDCProvider struct {
	GoogleOIDC *oidc.Provider
}

var (
	// OAuthProviders is a global variable that contains instance for all enabled the OAuth providers
	OAuthProviders OAuthProvider
	// OIDCProviders is a global variable that contains instance for all enabled the OpenID providers
	OIDCProviders OIDCProvider
	// oauthMu guards reads/writes of the provider globals so an admin-triggered
	// ReloadOAuth doesn't race with in-flight login/callback handlers.
	oauthMu sync.RWMutex
)

// InitOAuth builds the OAuth providers from the resolved credentials
// (DB-first, ENV-fallback). Safe to call again via ReloadOAuth after an admin
// updates credentials.
func InitOAuth() error {
	return ReloadOAuth()
}

// ReloadOAuth rebuilds the provider globals from current credentials. Called at
// startup and after an admin saves new credentials, so changes take effect
// without a process restart.
func ReloadOAuth() error {
	ctx := context.Background()
	creds := resolveCreds()
	backendDomain := os.Getenv("BACKEND_DOMAIN")
	protocol := httpProtocol(backendDomain)

	var googleConfig, googleCalConfig, githubConfig *oauth2.Config
	var googleOIDC *oidc.Provider

	if creds.GoogleClientID != "" && creds.GoogleClientSecret != "" {
		p, err := oidc.NewProvider(ctx, "https://accounts.google.com")
		if err != nil {
			return err
		}
		googleOIDC = p

		googleConfig = &oauth2.Config{
			ClientID:     creds.GoogleClientID,
			ClientSecret: creds.GoogleClientSecret,
			RedirectURL:  protocol + backendDomain + "/oauth_callback/google",
			Endpoint:     p.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		}

		googleCalConfig = &oauth2.Config{
			ClientID:     creds.GoogleClientID,
			ClientSecret: creds.GoogleClientSecret,
			RedirectURL:  protocol + backendDomain + "/user/integration/google-calendar/callback", // Matches router
			Endpoint:     p.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email", "https://www.googleapis.com/auth/calendar"},
		}
	}

	if creds.GithubClientID != "" && creds.GithubClientSecret != "" {
		githubConfig = &oauth2.Config{
			ClientID:     creds.GithubClientID,
			ClientSecret: creds.GithubClientSecret,
			RedirectURL:  protocol + backendDomain + "/oauth_callback/github",
			Endpoint:     githubOAuth2.Endpoint,
			Scopes:       []string{"read:user", "user:email"},
		}
	}

	oauthMu.Lock()
	OIDCProviders.GoogleOIDC = googleOIDC
	OAuthProviders.GoogleConfig = googleConfig
	OAuthProviders.GoogleCalendarConfig = googleCalConfig
	OAuthProviders.GithubConfig = githubConfig
	oauthMu.Unlock()

	return nil
}

// GoogleConfig / GithubConfig / GoogleCalendarConfig / GoogleOIDCProvider are
// race-safe accessors for handlers that read the providers concurrently with a
// reload. Existing call sites that read the globals directly still work; new
// code should prefer these.
func GoogleConfig() *oauth2.Config {
	oauthMu.RLock()
	defer oauthMu.RUnlock()
	return OAuthProviders.GoogleConfig
}

func GithubConfig() *oauth2.Config {
	oauthMu.RLock()
	defer oauthMu.RUnlock()
	return OAuthProviders.GithubConfig
}

func GoogleCalendarConfig() *oauth2.Config {
	oauthMu.RLock()
	defer oauthMu.RUnlock()
	return OAuthProviders.GoogleCalendarConfig
}

func GoogleOIDCProvider() *oidc.Provider {
	oauthMu.RLock()
	defer oauthMu.RUnlock()
	return OIDCProviders.GoogleOIDC
}
