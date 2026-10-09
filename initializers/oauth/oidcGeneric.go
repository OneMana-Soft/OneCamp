package oauth

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type GenericOIDCProvider struct {
	Config   *oauth2.Config
	Verifier *oidc.IDTokenVerifier
}

var OIDCGeneric GenericOIDCProvider

// InitGenericOIDC sets OIDC sign-in up at boot, when OIDC_ENABLED is on. The
// issuer is discovered once, here, so an error leaves OIDC sign-in off until
// the API restarts, and the error is the only record of why: the caller logs
// it (cmd/server).
func InitGenericOIDC() error {
	if !helpers.EnvFlag("OIDC_ENABLED") {
		return nil
	}

	ctx := context.Background()
	issuer := os.Getenv("OIDC_ISSUER_URL")
	clientID := os.Getenv("OIDC_CLIENT_ID")
	clientSecret := os.Getenv("OIDC_CLIENT_SECRET")
	backendDomain := os.Getenv("BACKEND_DOMAIN")

	// Named, so the boot log says which one to set.
	var missing []string
	for _, v := range [][2]string{
		{"OIDC_ISSUER_URL", issuer}, {"OIDC_CLIENT_ID", clientID},
		{"OIDC_CLIENT_SECRET", clientSecret}, {"BACKEND_DOMAIN", backendDomain},
	} {
		if v[1] == "" {
			missing = append(missing, v[0])
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("OIDC_ENABLED but missing required env: %s", strings.Join(missing, ", "))
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
