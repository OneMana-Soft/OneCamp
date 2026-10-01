package adapter

// App platform DTOs. Mirrors the AI-provider convention: secrets (signing
// secret, OAuth client secret, API keys) are NEVER returned to the FE — only
// boolean "has_*" flags. This is the border type between the admin HTTP layer
// and the FE app-directory UI.

// AppView is an installed app as shown to the admin (no secrets).
type AppView struct {
	ID               string           `json:"id"`
	Slug             string           `json:"slug"`
	Name             string           `json:"name"`
	Description      string           `json:"description,omitempty"`
	IconURL          string           `json:"icon_url,omitempty"`
	Kind             string           `json:"kind"` // builtin | external | oauth
	HandlerURL       string           `json:"handler_url,omitempty"`
	HasSigningSecret bool             `json:"has_signing_secret"`
	HasOAuthConfig   bool             `json:"has_oauth_config"`
	HasAPIKey        bool             `json:"has_api_key"`           // an api_key secret is stored
	SecretKeys       []string         `json:"secret_keys,omitempty"` // names of stored secrets (values never returned)
	IsEnabled        bool             `json:"is_enabled"`
	IsConnected      bool             `json:"is_connected"` // OAuth token present (kind=oauth)
	Commands         []AppCommandView `json:"commands"`
	CreatedAt        string           `json:"created_at,omitempty"`
	UpdatedAt        string           `json:"updated_at,omitempty"`
}

// AppCommandView is a command provided by an app, shown in the admin UI.
type AppCommandView struct {
	ID           string `json:"id"`
	Command      string `json:"command"`
	Description  string `json:"description"`
	UsageHint    string `json:"usage_hint,omitempty"`
	ExecMode     string `json:"exec_mode"`
	ResponseType string `json:"response_type"`
	IsEnabled    bool   `json:"is_enabled"`
}

// CreateAppRequest installs a new app. For OAuth apps, oauth_config carries the
// client id/secret/auth+token URLs/scopes; the secret is encrypted at rest.
type CreateAppRequest struct {
	Slug          string            `json:"slug"`
	Name          string            `json:"name"`
	Description   string            `json:"description,omitempty"`
	IconURL       string            `json:"icon_url,omitempty"`
	Kind          string            `json:"kind"` // external | oauth
	HandlerURL    string            `json:"handler_url,omitempty"`
	SigningSecret string            `json:"signing_secret,omitempty"` // omit to auto-generate
	OAuthConfig   *AppOAuthConfig   `json:"oauth_config,omitempty"`
	Config        map[string]string `json:"config,omitempty"`  // non-secret per-app config
	Secrets       map[string]string `json:"secrets,omitempty"` // secret config (encrypted): api keys etc.
	Commands      []AppCommandInput `json:"commands,omitempty"`
}

// UpdateAppRequest edits an app. Omitted fields are left unchanged. Secret
// fields follow the AI-provider convention: omit = keep, "" = clear, value = set.
type UpdateAppRequest struct {
	Name          *string           `json:"name,omitempty"`
	Description   *string           `json:"description,omitempty"`
	IconURL       *string           `json:"icon_url,omitempty"`
	HandlerURL    *string           `json:"handler_url,omitempty"`
	SigningSecret *string           `json:"signing_secret,omitempty"`
	IsEnabled     *bool             `json:"is_enabled,omitempty"`
	OAuthConfig   *AppOAuthConfig   `json:"oauth_config,omitempty"`
	Config        map[string]string `json:"config,omitempty"`
	Secrets       map[string]string `json:"secrets,omitempty"`
	Commands      []AppCommandInput `json:"commands,omitempty"` // full replace when present
}

// AppOAuthConfig is the OAuth client config for an oauth-kind app. ClientSecret
// is write-only (never serialized back).
type AppOAuthConfig struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret,omitempty"`
	AuthURL      string   `json:"auth_url"`
	TokenURL     string   `json:"token_url"`
	Scopes       []string `json:"scopes,omitempty"`
}

// AppCommandInput registers/updates one command for an app.
type AppCommandInput struct {
	Command       string `json:"command"`
	Description   string `json:"description"`
	UsageHint     string `json:"usage_hint,omitempty"`
	ExecMode      string `json:"exec_mode,omitempty"`     // defaults to external
	ResponseType  string `json:"response_type,omitempty"` // defaults to ephemeral
	HandlerURL    string `json:"handler_url,omitempty"`   // per-command override
	ScopeType     string `json:"scope_type,omitempty"`    // org | team | channel
	ScopeEntityID string `json:"scope_entity_id,omitempty"`
}

// TestAppRequest probes an app's handler URL connectivity.
type TestAppRequest struct {
	AppID      string `json:"app_id,omitempty"`
	HandlerURL string `json:"handler_url,omitempty"`
}

// AppTestResult is the outcome of the admin "Test" action — whether the app's
// stored credentials / handler actually work, with a human-readable message.
type AppTestResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// InstallFromManifestRequest installs an app from a Slack-style manifest URL or
// inline manifest JSON, enabling one-click install of third-party apps.
type InstallFromManifestRequest struct {
	ManifestURL  string `json:"manifest_url,omitempty"`
	ManifestJSON string `json:"manifest_json,omitempty"`
}

// OAuthInstallURLResponse carries the URL the admin is redirected to to
// authorize an OAuth app.
type OAuthInstallURLResponse struct {
	URL string `json:"url"`
}

// SetupField describes one credential/endpoint an admin must supply to finish
// configuring a marketplace app after one-click install.
type SetupField struct {
	Key      string `json:"key"`   // e.g. "api_key", "oauth_cred"
	Label    string `json:"label"` // human-readable
	Type     string `json:"type"`  // secret | handler_url | oauth_cred
	Required bool   `json:"required"`
}

// MarketplaceItem is a curated app-directory entry enriched with this
// workspace's install state, so the FE can render the right one-click action
// (Install / Set up / Uninstall) for each card.
type MarketplaceItem struct {
	Slug        string       `json:"slug"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Category    string       `json:"category"`
	IconURL     string       `json:"icon_url,omitempty"`
	Kind        string       `json:"kind"`
	Featured    bool         `json:"featured"`
	Commands    []string     `json:"commands"`
	Setup       []SetupField `json:"setup,omitempty"`
	SetupNote   string       `json:"setup_note,omitempty"`

	// Per-workspace state.
	Installed   bool   `json:"installed"`
	Enabled     bool   `json:"enabled"`
	NeedsSetup  bool   `json:"needs_setup"`
	IsConnected bool   `json:"is_connected"`
	AppID       string `json:"app_id,omitempty"`
}

// InstallTemplateRequest one-click-installs a curated app by slug.
type InstallTemplateRequest struct {
	Slug string `json:"slug"`
}
