package business

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	"github.com/akashc777/OneCamp/helpers"
	integrationModel "github.com/akashc777/OneCamp/models/postgres/Integration"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
	"github.com/google/uuid"
)

// appBusiness.go — the generic third-party app platform. An app bundles:
//   - metadata (slug, name, icon, kind)
//   - a signing secret (HMAC for outbound command dispatch, encrypted at rest)
//   - optional OAuth config (encrypted client secret) for apps that need a
//     per-workspace token (Zoom, Jira, Google-style)
//   - optional non-secret config + encrypted secret config (e.g. a Giphy API key)
//   - a set of slash commands it provides
//
// This is provider-agnostic: Giphy, Zoom, Jira, a custom internal bot — all
// install through the same path and dispatch through the same SSRF-guarded,
// HMAC-signed forwarder in dispatcher.go.

const appConfigProvider = "app_config" // integrations.provider for app secret bag
const appOAuthProviderPrefix = "app_oauth:"

// secretConfig is the JSON shape we encrypt into integrations for an app's
// secret bag (api keys, etc.) and oauth client secret.
type appSecretBag struct {
	OAuthClientSecret string            `json:"oauth_client_secret,omitempty"`
	Secrets           map[string]string `json:"secrets,omitempty"`
}

// CreateApp installs a new app: encrypts secrets, persists the app + its
// commands, and busts the command catalog cache so the new commands appear.
func CreateApp(ctx context.Context, req commandAdapter.CreateAppRequest, installedBy uuid.UUID) (*commandAdapter.AppView, error) {
	slug := normalizeSlug(req.Slug)
	if slug == "" || req.Name == "" {
		return nil, fmt.Errorf("slug and name are required")
	}
	kind := req.Kind
	if kind == "" {
		kind = slashModel.AppKindExternal
	}

	// Generate a signing secret if none provided (apps almost always want one).
	signing := req.SigningSecret
	if signing == "" && kind != slashModel.AppKindBuiltin {
		signing = generateSecret(32)
	}
	encSigning, err := helpers.EncryptSecret(signing)
	if err != nil {
		return nil, fmt.Errorf("encrypt signing secret: %w", err)
	}

	app := &slashModel.App{
		Slug:        slug,
		Name:        req.Name,
		Kind:        kind,
		IsEnabled:   true,
		InstalledBy: &installedBy,
	}
	if req.Description != "" {
		app.Description = &req.Description
	}
	if req.IconURL != "" {
		app.IconUrl = &req.IconURL
	}
	if req.HandlerURL != "" {
		if _, verr := helpers.ValidateOutboundURL(req.HandlerURL, false); verr != nil {
			return nil, fmt.Errorf("handler_url rejected: %w", verr)
		}
		app.HandlerUrl = &req.HandlerURL
	}
	if encSigning != "" {
		app.SigningSecret = &encSigning
	}
	// Public (non-secret) OAuth config + app config persist as JSON on the app row.
	if req.OAuthConfig != nil {
		pub := publicOAuthConfig(req.OAuthConfig)
		if b, mErr := json.Marshal(pub); mErr == nil {
			s := string(b)
			app.OAuthConfig = &s
		}
	}
	if len(req.Config) > 0 {
		if b, mErr := json.Marshal(req.Config); mErr == nil {
			s := string(b)
			app.Config = &s
		}
	}

	appID, err := slashModel.CreateApp(ctx, app)
	if err != nil {
		return nil, err
	}

	// Persist secret bag (oauth client secret + arbitrary api keys) encrypted
	// in the integrations table keyed by the app id.
	if err := saveAppSecrets(ctx, appID, req.OAuthConfig, req.Secrets); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Command/CreateApp saveAppSecrets err: %+v", err)
		return nil, fmt.Errorf("failed to save credentials: %w", err)
	}

	// Register the app's commands. Built-in apps (Giphy, OneCamp AI) are backed
	// by in-process handlers whose commands are already seeded as org-scoped
	// built-ins (seed.go); creating app-linked rows for them would collide on
	// the unique (command, scope) index and silently fail. Their command list
	// is surfaced from the template in buildAppViewFrom instead.
	if kind != slashModel.AppKindBuiltin {
		if err := syncAppCommands(ctx, appID, req.Commands, app.HandlerUrl); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Command/CreateApp syncAppCommands err: %+v", err)
		}
	}

	bumpCatalogVersion(ctx)
	return buildAppView(ctx, appID)
}

// UpdateApp applies partial edits. Secret fields follow omit=keep / ""=clear /
// value=set semantics, matching the AI provider convention.
func UpdateApp(ctx context.Context, appID uuid.UUID, req commandAdapter.UpdateAppRequest) (*commandAdapter.AppView, error) {
	existing, err := slashModel.GetAppByID(ctx, appID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("app not found")
	}

	fields := map[string]interface{}{}
	if req.Name != nil {
		fields["name"] = *req.Name
	}
	if req.Description != nil {
		fields["description"] = nullableString(*req.Description)
	}
	if req.IconURL != nil {
		fields["icon_url"] = nullableString(*req.IconURL)
	}
	if req.HandlerURL != nil {
		if *req.HandlerURL != "" {
			if _, verr := helpers.ValidateOutboundURL(*req.HandlerURL, false); verr != nil {
				return nil, fmt.Errorf("handler_url rejected: %w", verr)
			}
		}
		fields["handler_url"] = nullableString(*req.HandlerURL)
	}
	if req.IsEnabled != nil {
		fields["is_enabled"] = *req.IsEnabled
	}
	if req.SigningSecret != nil {
		enc, encErr := helpers.EncryptSecret(*req.SigningSecret)
		if encErr != nil {
			return nil, fmt.Errorf("encrypt signing secret: %w", encErr)
		}
		fields["signing_secret"] = nullableString(enc)
	}
	if req.OAuthConfig != nil {
		pub := publicOAuthConfig(req.OAuthConfig)
		if b, mErr := json.Marshal(pub); mErr == nil {
			fields["oauth_config"] = string(b)
		}
	}
	if req.Config != nil {
		if b, mErr := json.Marshal(req.Config); mErr == nil {
			fields["config"] = string(b)
		}
	}

	if len(fields) > 0 {
		if err := slashModel.UpdateApp(ctx, appID, fields); err != nil {
			return nil, err
		}
	}

	// Secrets (oauth client secret + api keys): merge when provided. Surface a
	// failure to the caller — a swallowed error here is exactly what made a
	// saved API key silently not persist while the UI reported success.
	if req.OAuthConfig != nil || len(req.Secrets) > 0 {
		if err := saveAppSecrets(ctx, appID, req.OAuthConfig, req.Secrets); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Command/UpdateApp saveAppSecrets err: %+v", err)
			return nil, fmt.Errorf("failed to save credentials: %w", err)
		}
	}

	// Full command replace when commands provided. Skip for built-in apps —
	// their commands are seeded org-scoped built-ins, not app-linked rows, so a
	// replace here would both no-op the delete and collide on insert.
	if req.Commands != nil && existing.Kind != slashModel.AppKindBuiltin {
		handler := existing.HandlerUrl
		if req.HandlerURL != nil && *req.HandlerURL != "" {
			handler = req.HandlerURL
		}
		_ = slashModel.DeleteCommandsByApp(ctx, appID)
		if err := syncAppCommands(ctx, appID, req.Commands, handler); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Command/UpdateApp syncAppCommands err: %+v", err)
		}
	}

	bumpCatalogVersion(ctx)
	return buildAppView(ctx, appID)
}

// SetAppEnabled toggles an app and its commands' visibility.
func SetAppEnabled(ctx context.Context, appID uuid.UUID, enabled bool) error {
	if err := slashModel.SetAppEnabled(ctx, appID, enabled); err != nil {
		return err
	}
	bumpCatalogVersion(ctx)
	return nil
}

// DeleteApp soft-deletes an app, its commands, and its secret bag.
func DeleteApp(ctx context.Context, appID uuid.UUID) error {
	if err := slashModel.SoftDeleteApp(ctx, appID); err != nil {
		return err
	}
	// Remove the encrypted secret bag.
	delQ := `DELETE FROM integrations WHERE entity_type = 'app' AND entity_id = $1 AND provider = $2`
	_ = integrationModel.DeleteIntegration(delQ, "app", appID, appConfigProvider)
	bumpCatalogVersion(ctx)
	return nil
}

// ListApps returns every installed app for the admin directory (no secrets).
func ListApps(ctx context.Context) ([]*commandAdapter.AppView, error) {
	apps, err := slashModel.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*commandAdapter.AppView, 0, len(apps))
	for _, a := range apps {
		view, vErr := buildAppViewFrom(ctx, a)
		if vErr != nil {
			continue
		}
		out = append(out, view)
	}
	return out, nil
}

// GetApp returns a single app view.
func GetApp(ctx context.Context, appID uuid.UUID) (*commandAdapter.AppView, error) {
	return buildAppView(ctx, appID)
}

// GetAppSecret returns a decrypted secret value from an app's secret bag (e.g.
// the Giphy API key). Used by built-in app adapters at dispatch time.
func GetAppSecret(ctx context.Context, appID uuid.UUID, key string) (string, error) {
	bag, err := loadAppSecrets(ctx, appID)
	if err != nil || bag == nil {
		return "", err
	}
	return bag.Secrets[key], nil
}

// --- internal helpers ---

func saveAppSecrets(ctx context.Context, appID uuid.UUID, oauth *commandAdapter.AppOAuthConfig, secrets map[string]string) error {
	bag, _ := loadAppSecrets(ctx, appID)
	if bag == nil {
		bag = &appSecretBag{Secrets: map[string]string{}}
	}
	if bag.Secrets == nil {
		bag.Secrets = map[string]string{}
	}
	if oauth != nil && oauth.ClientSecret != "" {
		bag.OAuthClientSecret = strings.TrimSpace(oauth.ClientSecret)
	}
	for k, v := range secrets {
		// Trim whitespace/newlines that commonly sneak in on paste — a
		// trailing newline in an API key is the #1 cause of a provider 401.
		v = strings.TrimSpace(v)
		if v == "" {
			delete(bag.Secrets, k) // "" clears a secret
		} else {
			bag.Secrets[k] = v
		}
	}

	plain, err := json.Marshal(bag)
	if err != nil {
		return err
	}
	enc, err := helpers.EncryptSecret(string(plain))
	if err != nil {
		return err
	}

	// NOTE: integrationModel.UpsertIntegration ALWAYS binds 9 positional args in
	// a fixed order (entity_type, entity_id, provider, access_token,
	// refresh_token, webhook_url, metadata, expires_at, updated_at). The query
	// MUST therefore reference $1..$9 in that exact order — a shorter query
	// (e.g. $1..$5) silently fails at the DB, which is why a saved Giphy key
	// never persisted and the app stayed "needs setup".
	const upsert = `
		INSERT INTO integrations (entity_type, entity_id, provider, access_token, refresh_token, webhook_url, metadata, expires_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (entity_type, entity_id, provider)
		DO UPDATE SET access_token = EXCLUDED.access_token, updated_at = EXCLUDED.updated_at`
	return integrationModel.UpsertIntegration(upsert, "app", appID, appConfigProvider, &enc, nil, nil, nil, nil, time.Now())
}

func loadAppSecrets(ctx context.Context, appID uuid.UUID) (*appSecretBag, error) {
	const q = `SELECT id, entity_type, entity_id, provider, access_token, refresh_token, sync_token,
		webhook_url, metadata, expires_at, task_sync_enabled, created_at, updated_at
		FROM integrations WHERE entity_type = $1 AND entity_id = $2 AND provider = $3`
	rec, err := integrationModel.GetIntegration(q, "app", appID, appConfigProvider)
	if err != nil || rec == nil || rec.AccessToken == nil {
		return nil, err
	}
	plain, err := helpers.DecryptSecret(*rec.AccessToken)
	if err != nil {
		return nil, err
	}
	var bag appSecretBag
	if err := json.Unmarshal([]byte(plain), &bag); err != nil {
		return nil, err
	}
	return &bag, nil
}

// syncAppCommands registers an app's command set. Each command defaults to the
// external exec mode (dispatched to the app handler) unless overridden.
func syncAppCommands(ctx context.Context, appID uuid.UUID, cmds []commandAdapter.AppCommandInput, defaultHandler *string) error {
	for _, ci := range cmds {
		name := normalizeCommand(ci.Command)
		if name == "" {
			continue
		}
		execMode := ci.ExecMode
		if execMode == "" {
			execMode = slashModel.ExecExternal
		}
		respType := ci.ResponseType
		if respType == "" {
			respType = "ephemeral"
		}
		scopeType := ci.ScopeType
		if scopeType == "" {
			scopeType = "org"
		}
		cmd := &slashModel.SlashCommand{
			Command:      name,
			AppId:        &appID,
			Description:  ci.Description,
			ExecMode:     execMode,
			ResponseType: respType,
			ScopeType:    scopeType,
			IsEnabled:    true,
		}
		if ci.UsageHint != "" {
			cmd.UsageHint = &ci.UsageHint
		}
		if ci.HandlerURL != "" {
			cmd.HandlerUrl = &ci.HandlerURL
		} else if defaultHandler != nil {
			cmd.HandlerUrl = defaultHandler
		}
		if ci.ScopeEntityID != "" {
			if id, err := uuid.Parse(ci.ScopeEntityID); err == nil {
				cmd.ScopeEntityId = &id
			}
		}
		if err := slashModel.CreateAppCommand(ctx, cmd); err != nil {
			return err
		}
	}
	return nil
}

func buildAppView(ctx context.Context, appID uuid.UUID) (*commandAdapter.AppView, error) {
	app, err := slashModel.GetAppByID(ctx, appID)
	if err != nil || app == nil {
		return nil, err
	}
	return buildAppViewFrom(ctx, app)
}

func buildAppViewFrom(ctx context.Context, app *slashModel.App) (*commandAdapter.AppView, error) {
	view := &commandAdapter.AppView{
		ID:               app.Id.String(),
		Slug:             app.Slug,
		Name:             app.Name,
		Kind:             app.Kind,
		IsEnabled:        app.IsEnabled,
		HasSigningSecret: app.SigningSecret != nil && *app.SigningSecret != "",
		HasOAuthConfig:   app.OAuthConfig != nil && *app.OAuthConfig != "",
		CreatedAt:        app.CreatedAt.Format(time.RFC3339),
		UpdatedAt:        app.UpdatedAt.Format(time.RFC3339),
	}
	if app.Description != nil {
		view.Description = *app.Description
	}
	if app.IconUrl != nil {
		view.IconURL = *app.IconUrl
	}
	if app.HandlerUrl != nil {
		view.HandlerURL = *app.HandlerUrl
	}

	// OAuth connection status: token present in integrations under the app's
	// oauth provider key.
	if app.Kind == slashModel.AppKindOAuth {
		view.IsConnected = appOAuthConnected(ctx, app.Id)
	}

	// Surface which secrets are configured (names only, never values) so the
	// admin UI can show "API key set" instead of an always-empty password field
	// that looks unsaved.
	if bag, _ := loadAppSecrets(ctx, app.Id); bag != nil {
		for k, v := range bag.Secrets {
			if v == "" {
				continue
			}
			view.SecretKeys = append(view.SecretKeys, k)
			if k == "api_key" {
				view.HasAPIKey = true
			}
		}
		sort.Strings(view.SecretKeys)
	}

	cmds, _ := slashModel.ListCommandsByApp(ctx, app.Id)
	// Always a non-nil slice so it marshals to [] not null (the FE does
	// app.commands.length / .map and would crash on null).
	view.Commands = make([]commandAdapter.AppCommandView, 0, len(cmds))
	for _, c := range cmds {
		cv := commandAdapter.AppCommandView{
			ID:           c.Id.String(),
			Command:      c.Command,
			Description:  c.Description,
			ExecMode:     c.ExecMode,
			ResponseType: c.ResponseType,
			IsEnabled:    c.IsEnabled,
		}
		if c.UsageHint != nil {
			cv.UsageHint = *c.UsageHint
		}
		view.Commands = append(view.Commands, cv)
	}

	// Built-in apps own no app-linked command rows (their commands live as
	// seeded org-scoped built-ins), so surface the command list from the
	// marketplace template — otherwise the editor shows "No commands yet" for a
	// fully-working app like Giphy.
	if app.Kind == slashModel.AppKindBuiltin && len(view.Commands) == 0 {
		if t, ok := getTemplate(app.Slug); ok {
			for _, c := range t.Commands {
				cv := commandAdapter.AppCommandView{
					Command:      c.Command,
					Description:  c.Description,
					ExecMode:     c.ExecMode,
					ResponseType: c.ResponseType,
					IsEnabled:    true,
					UsageHint:    c.UsageHint,
				}
				view.Commands = append(view.Commands, cv)
			}
		}
	}
	return view, nil
}

func appOAuthConnected(ctx context.Context, appID uuid.UUID) bool {
	const q = `SELECT id, entity_type, entity_id, provider, access_token, refresh_token, sync_token,
		webhook_url, metadata, expires_at, task_sync_enabled, created_at, updated_at
		FROM integrations WHERE entity_type = $1 AND entity_id = $2 AND provider = $3`
	rec, err := integrationModel.GetIntegration(q, "app", appID, appOAuthProviderPrefix+"token")
	return err == nil && rec != nil && rec.AccessToken != nil && *rec.AccessToken != ""
}

func publicOAuthConfig(c *commandAdapter.AppOAuthConfig) map[string]interface{} {
	return map[string]interface{}{
		"client_id": c.ClientID,
		"auth_url":  c.AuthURL,
		"token_url": c.TokenURL,
		"scopes":    c.Scopes,
	}
}

func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func normalizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "-")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func generateSecret(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return uuid.NewString() + uuid.NewString()
	}
	return hex.EncodeToString(buf)
}
