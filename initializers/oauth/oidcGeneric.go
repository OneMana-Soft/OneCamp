package oauth

import (
	"context"
	"fmt"
	"os"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type GenericOIDCProvider struct {
	Config   *oauth2.Config
	Verifier *oidc.IDTokenVerifier
}

var OIDCGeneric GenericOIDCProvider

func InitGenericOIDC() error {
	if os.Getenv("OIDC_ENABLED") != "true" {
		return nil
	}

	ctx := context.Background()
	issuer := os.Getenv("OIDC_ISSUER_URL")
	clientID := os.Getenv("OIDC_CLIENT_ID")
	clientSecret := os.Getenv("OIDC_CLIENT_SECRET")
	backendDomain := os.Getenv("BACKEND_DOMAIN")

	if issuer == "" || clientID == "" || clientSecret == "" || backendDomain == "" {
		return fmt.Errorf("missing required OIDC environment variables")
	}

	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return fmt.Errorf("failed to discover OIDC provider at %s: %w", issuer, err)
	}

	protocol := "https://"
	if os.Getenv("COOKIE_SECURE") == "false" || stringsContainsLocalhost(backendDomain) {
		protocol = "http://"
	}

	OIDCGeneric.Config = &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  protocol + backendDomain + "/oauth_callback/oidc",
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}

	OIDCGeneric.Verifier = provider.Verifier(&oidc.Config{ClientID: clientID})
	return nil
}

func stringsContainsLocalhost(s string) bool {
	return len(s) > 0 && (s == "localhost" || s == "127.0.0.1" || (len(s) >= 9 && s[:9] == "localhost") || (len(s) >= 9 && s[:9] == "127.0.0.1"))
}
