package business

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

// githubConfig.go — admin-managed GitHub App credentials.
//
// Credentials (OAuth App client id/secret + webhook secret) move from env into
// the admin Integrations tab, stored in system_configs. Secrets are encrypted
// at rest with helpers.EncryptSecret (AES-256-GCM, KEK from APP_SECRET_KEK).
//
// Resolution is DB-first with ENV fallback so existing deployments that still
// set GITHUB_APP_CLIENT_ID / GITHUB_APP_CLIENT_SECRET / GITHUB_WEBHOOK_SECRET
// keep working with zero changes; once an admin saves credentials in the UI,
// the DB values take precedence. The encryption KEK itself stays in env — you
// cannot bootstrap encryption from the database you're encrypting.

const (
	cfgGitHubClientID      = "github_app_client_id"
	cfgGitHubClientSecret  = "github_app_client_secret" // encrypted
	cfgGitHubWebhookSecret = "github_webhook_secret"    // encrypted
)

// ghConfigCache is a tiny in-process cache so the OAuth + webhook hot paths
// don't hit Postgres on every request. Invalidated on save.
var (
	ghConfigMu      sync.RWMutex
	ghConfigCache   *GitHubAppConfig
	ghConfigExpires time.Time
)

const ghConfigTTL = 60 * time.Second

// GitHubAppConfig is the resolved GitHub App credential set.
type GitHubAppConfig struct {
	ClientID      string
	ClientSecret  string
	WebhookSecret string
	// Source flags for the admin UI / diagnostics.
	FromDB bool
}

// GetGitHubAppConfig resolves credentials DB-first, ENV-fallback, with a short
// in-process cache. Never returns an error — missing config yields empty
// strings, and callers already treat empty client id as "not configured".
func GetGitHubAppConfig(ctx context.Context) *GitHubAppConfig {
	ghConfigMu.RLock()
	if ghConfigCache != nil && time.Now().Before(ghConfigExpires) {
		c := *ghConfigCache
		ghConfigMu.RUnlock()
		return &c
	}
	ghConfigMu.RUnlock()

	cfg := resolveGitHubAppConfig(ctx)

	ghConfigMu.Lock()
	ghConfigCache = cfg
	ghConfigExpires = time.Now().Add(ghConfigTTL)
	ghConfigMu.Unlock()

	c := *cfg
	return &c
}

func resolveGitHubAppConfig(ctx context.Context) *GitHubAppConfig {
	cfg := &GitHubAppConfig{}

	// Pull the three keys in one query. Guard against an uninitialized DB
	// (unit tests, early boot) so we degrade gracefully to env-only config
	// instead of panicking on a nil connection.
	if postgresInit.DBConn != nil && postgresInit.DBConn.SqlDB != nil {
		rows, err := configModel.GetMultipleConfigsByKeys([]string{
			cfgGitHubClientID, cfgGitHubClientSecret, cfgGitHubWebhookSecret,
		})
		if err == nil {
			for _, row := range rows {
				switch row.Key {
				case cfgGitHubClientID:
					if row.Value != "" {
						cfg.ClientID = row.Value
						cfg.FromDB = true
					}
				case cfgGitHubClientSecret:
					if dec, derr := helpers.DecryptSecret(row.Value); derr == nil && dec != "" {
						cfg.ClientSecret = dec
						cfg.FromDB = true
					}
				case cfgGitHubWebhookSecret:
					if dec, derr := helpers.DecryptSecret(row.Value); derr == nil && dec != "" {
						cfg.WebhookSecret = dec
					}
				}
			}
		}
	}

	// ENV fallback per-field (so a partially-configured DB still falls back for
	// the missing pieces — e.g. webhook secret left in env during migration).
	if cfg.ClientID == "" {
		cfg.ClientID = helpers.FirstNonBlank(os.Getenv("GITHUB_APP_CLIENT_ID"), os.Getenv("GITHUB_CLIENT_ID"))
	}
	if cfg.ClientSecret == "" {
		cfg.ClientSecret = helpers.FirstNonBlank(os.Getenv("GITHUB_APP_CLIENT_SECRET"), os.Getenv("GITHUB_CLIENT_SECRET"))
	}
	if cfg.WebhookSecret == "" {
		cfg.WebhookSecret = os.Getenv("GITHUB_WEBHOOK_SECRET")
	}
	return cfg
}

// SaveGitHubAppConfig persists admin-entered credentials. Secret fields follow
// omit=keep / ""=clear / value=set semantics (matching the AI provider and app
// platform conventions). clientID is non-secret and stored as-is.
//
// Pass nil for a field to leave it unchanged.
func SaveGitHubAppConfig(ctx context.Context, clientID, clientSecret, webhookSecret *string) error {
	if clientID != nil {
		if err := configModel.UpsertConfig(cfgGitHubClientID, *clientID); err != nil {
			return err
		}
	}
	if clientSecret != nil {
		enc, err := helpers.EncryptSecret(*clientSecret)
		if err != nil {
			return err
		}
		if err := configModel.UpsertConfig(cfgGitHubClientSecret, enc); err != nil {
			return err
		}
	}
	if webhookSecret != nil {
		enc, err := helpers.EncryptSecret(*webhookSecret)
		if err != nil {
			return err
		}
		if err := configModel.UpsertConfig(cfgGitHubWebhookSecret, enc); err != nil {
			return err
		}
	}
	invalidateGitHubConfigCache()
	return nil
}

func invalidateGitHubConfigCache() {
	ghConfigMu.Lock()
	ghConfigCache = nil
	ghConfigExpires = time.Time{}
	ghConfigMu.Unlock()
}

// GitHubConfigStatus is the admin-facing view of GitHub App config. Secrets are
// never returned — only booleans indicating whether each is set, plus the
// non-secret client id and where the config is sourced from.
type GitHubConfigStatus struct {
	ClientID         string `json:"client_id"`
	HasClientSecret  bool   `json:"has_client_secret"`
	HasWebhookSecret bool   `json:"has_webhook_secret"`
	Configured       bool   `json:"configured"`
	Source           string `json:"source"` // "db" | "env" | "none"
}

// GetGitHubConfigStatus returns the redacted config view for the admin UI.
func GetGitHubConfigStatus(ctx context.Context) *GitHubConfigStatus {
	cfg := GetGitHubAppConfig(ctx)
	source := "none"
	if cfg.FromDB {
		source = "db"
	} else if cfg.ClientID != "" || cfg.ClientSecret != "" || cfg.WebhookSecret != "" {
		source = "env"
	}
	return &GitHubConfigStatus{
		ClientID:         cfg.ClientID,
		HasClientSecret:  cfg.ClientSecret != "",
		HasWebhookSecret: cfg.WebhookSecret != "",
		Configured:       cfg.ClientID != "" && cfg.ClientSecret != "",
		Source:           source,
	}
}
