package oauth

import (
	"os"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

// credentials.go — admin-managed OAuth credentials (Google login + Google
// Calendar share one Google OAuth client; GitHub login is separate).
//
// Resolution is DB-first with ENV fallback, mirroring the GitHub App config:
// existing deployments that set GOOGLE_CLIENT_ID / GITHUB_CLIENT_ID in env keep
// working unchanged; once an admin saves credentials in the UI, the DB values
// take precedence. Secrets are encrypted at rest with helpers.EncryptSecret.
//
// This lives in the oauth package (not business) so the startup InitOAuth and
// the admin reload path share one resolver without an import cycle — it depends
// only on the low-level config model + helpers.

const (
	cfgGoogleClientID     = "google_oauth_client_id"
	cfgGoogleClientSecret = "google_oauth_client_secret" // encrypted
	cfgGithubClientID     = "github_login_client_id"
	cfgGithubClientSecret = "github_login_client_secret" // encrypted
)

type resolvedCreds struct {
	GoogleClientID     string
	GoogleClientSecret string
	GithubClientID     string
	GithubClientSecret string
	GoogleFromDB       bool
	GithubFromDB       bool
}

func resolveCreds() resolvedCreds {
	c := resolvedCreds{}

	if postgresInit.DBConn != nil && postgresInit.DBConn.SqlDB != nil {
		rows, err := configModel.GetMultipleConfigsByKeys([]string{
			cfgGoogleClientID, cfgGoogleClientSecret, cfgGithubClientID, cfgGithubClientSecret,
		})
		if err == nil {
			for _, row := range rows {
				switch row.Key {
				case cfgGoogleClientID:
					if row.Value != "" {
						c.GoogleClientID = row.Value
						c.GoogleFromDB = true
					}
				case cfgGoogleClientSecret:
					if dec, derr := helpers.DecryptSecret(row.Value); derr == nil && dec != "" {
						c.GoogleClientSecret = dec
						c.GoogleFromDB = true
					}
				case cfgGithubClientID:
					if row.Value != "" {
						c.GithubClientID = row.Value
						c.GithubFromDB = true
					}
				case cfgGithubClientSecret:
					if dec, derr := helpers.DecryptSecret(row.Value); derr == nil && dec != "" {
						c.GithubClientSecret = dec
						c.GithubFromDB = true
					}
				}
			}
		}
	}

	// Per-field ENV fallback.
	if c.GoogleClientID == "" {
		c.GoogleClientID = os.Getenv("GOOGLE_CLIENT_ID")
	}
	if c.GoogleClientSecret == "" {
		c.GoogleClientSecret = os.Getenv("GOOGLE_CLIENT_SECRET")
	}
	if c.GithubClientID == "" {
		c.GithubClientID = os.Getenv("GITHUB_CLIENT_ID")
	}
	if c.GithubClientSecret == "" {
		c.GithubClientSecret = os.Getenv("GITHUB_CLIENT_SECRET")
	}
	return c
}

// OAuthConfigStatus is the redacted admin-facing view (no secrets).
type OAuthConfigStatus struct {
	GoogleClientID        string `json:"google_client_id"`
	GoogleHasClientSecret bool   `json:"google_has_client_secret"`
	GoogleConfigured      bool   `json:"google_configured"`
	GoogleSource          string `json:"google_source"` // db | env | none
	GithubClientID        string `json:"github_client_id"`
	GithubHasClientSecret bool   `json:"github_has_client_secret"`
	GithubConfigured      bool   `json:"github_configured"`
	GithubSource          string `json:"github_source"`
}

// GetOAuthConfigStatus returns the redacted login-OAuth config for the admin UI.
func GetOAuthConfigStatus() OAuthConfigStatus {
	c := resolveCreds()
	src := func(fromDB bool, id, secret string) string {
		if fromDB {
			return "db"
		}
		if id != "" || secret != "" {
			return "env"
		}
		return "none"
	}
	return OAuthConfigStatus{
		GoogleClientID:        c.GoogleClientID,
		GoogleHasClientSecret: c.GoogleClientSecret != "",
		GoogleConfigured:      c.GoogleClientID != "" && c.GoogleClientSecret != "",
		GoogleSource:          src(c.GoogleFromDB, c.GoogleClientID, c.GoogleClientSecret),
		GithubClientID:        c.GithubClientID,
		GithubHasClientSecret: c.GithubClientSecret != "",
		GithubConfigured:      c.GithubClientID != "" && c.GithubClientSecret != "",
		GithubSource:          src(c.GithubFromDB, c.GithubClientID, c.GithubClientSecret),
	}
}

// SaveGoogleOAuthConfig persists admin-entered Google login/calendar
// credentials (encrypted), then reloads the OAuth providers so the change
// takes effect without a restart. omit=keep / ""=clear / value=set.
func SaveGoogleOAuthConfig(clientID, clientSecret *string) error {
	if clientID != nil {
		if err := configModel.UpsertConfig(cfgGoogleClientID, *clientID); err != nil {
			return err
		}
	}
	if clientSecret != nil {
		enc, err := helpers.EncryptSecret(*clientSecret)
		if err != nil {
			return err
		}
		if err := configModel.UpsertConfig(cfgGoogleClientSecret, enc); err != nil {
			return err
		}
	}
	return ReloadOAuth()
}

// SaveGithubLoginConfig persists admin-entered GitHub *login* credentials
// (distinct from the GitHub App integration), then reloads OAuth providers.
func SaveGithubLoginConfig(clientID, clientSecret *string) error {
	if clientID != nil {
		if err := configModel.UpsertConfig(cfgGithubClientID, *clientID); err != nil {
			return err
		}
	}
	if clientSecret != nil {
		enc, err := helpers.EncryptSecret(*clientSecret)
		if err != nil {
			return err
		}
		if err := configModel.UpsertConfig(cfgGithubClientSecret, enc); err != nil {
			return err
		}
	}
	return ReloadOAuth()
}

// httpProtocol picks http for local/dev, https otherwise — shared by the
// provider builders.
func httpProtocol(backendDomain string) string {
	if strings.Contains(backendDomain, "localhost") ||
		strings.Contains(backendDomain, "127.0.0.1") ||
		strings.EqualFold(os.Getenv("COOKIE_SECURE"), "false") {
		return "http://"
	}
	return "https://"
}

// GoogleClientCreds returns the resolved Google OAuth client id/secret (DB-first,
// env fallback). Connectors (Gmail, Calendar) reuse the same Google OAuth client
// with their own scope sets, so they don't need separate credentials.
func GoogleClientCreds() (clientID, clientSecret string) {
	c := resolveCreds()
	return c.GoogleClientID, c.GoogleClientSecret
}

// GithubClientCreds returns the resolved GitHub *login* OAuth client id/secret.
// The GitHub connector reuses this client (requesting broader repo scopes at
// authorize time) so admins configure GitHub OAuth in exactly one place.
func GithubClientCreds() (clientID, clientSecret string) {
	c := resolveCreds()
	return c.GithubClientID, c.GithubClientSecret
}

// BackendProtocol exposes the http/https choice for the current backend domain
// so connector packages can build redirect URIs consistently.
func BackendProtocol(backendDomain string) string {
	return httpProtocol(backendDomain)
}
