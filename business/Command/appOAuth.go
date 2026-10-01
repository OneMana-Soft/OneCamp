package business

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	integrationModel "github.com/akashc777/OneCamp/models/postgres/Integration"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

// appOAuth.go — generic OAuth2 install flow for oauth-kind apps (Zoom, Jira,
// Google-style). The flow mirrors the GitHub integration:
//   1. BuildOAuthInstallURL → admin is redirected to the provider's auth URL
//      with a one-shot state nonce (Redis, GETDEL on consume → replay-safe).
//   2. HandleOAuthCallback → exchange code for tokens, encrypt + persist them
//      in integrations under provider "app_oauth:token".
//
// Client secret is read from the encrypted app secret bag; tokens are encrypted
// at rest with the same AES-256-GCM helper.

type publicOAuth struct {
	ClientID string   `json:"client_id"`
	AuthURL  string   `json:"auth_url"`
	TokenURL string   `json:"token_url"`
	Scopes   []string `json:"scopes"`
}

// BuildOAuthInstallURL returns the provider authorize URL for an oauth app.
func BuildOAuthInstallURL(ctx context.Context, appID uuid.UUID, redirectURI string) (string, error) {
	app, err := slashModel.GetAppByID(ctx, appID)
	if err != nil {
		return "", err
	}
	if app == nil {
		return "", fmt.Errorf("app not found")
	}
	if app.Kind != slashModel.AppKindOAuth || app.OAuthConfig == nil {
		return "", fmt.Errorf("app is not an OAuth app")
	}

	var cfg publicOAuth
	if err := json.Unmarshal([]byte(*app.OAuthConfig), &cfg); err != nil {
		return "", fmt.Errorf("invalid oauth config: %w", err)
	}
	if cfg.ClientID == "" || cfg.AuthURL == "" {
		return "", fmt.Errorf("oauth config incomplete")
	}

	// One-shot state nonce → appID, consumed (GETDEL) on callback.
	state := generateSecret(16)
	_ = redisStore.SetString(ctx, registry.CommandOAuthState, []string{state}, appID.String())

	u, err := url.Parse(cfg.AuthURL)
	if err != nil {
		return "", fmt.Errorf("invalid auth_url: %w", err)
	}
	q := u.Query()
	q.Set("client_id", cfg.ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	if len(cfg.Scopes) > 0 {
		q.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// HandleOAuthCallback exchanges the auth code for tokens and persists them.
// Returns the app id so the controller can redirect back to its page.
func HandleOAuthCallback(ctx context.Context, state, code, redirectURI string) (uuid.UUID, error) {
	if state == "" || code == "" {
		return uuid.Nil, fmt.Errorf("missing state or code")
	}
	appIDStr, found, _ := redisStore.GetDelString(ctx, registry.CommandOAuthState, []string{state})
	if !found || appIDStr == "" {
		return uuid.Nil, fmt.Errorf("invalid or expired state")
	}
	appID, err := uuid.Parse(appIDStr)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid app id in state")
	}

	app, err := slashModel.GetAppByID(ctx, appID)
	if err != nil || app == nil || app.OAuthConfig == nil {
		return appID, fmt.Errorf("app not found or not oauth")
	}
	var cfg publicOAuth
	if err := json.Unmarshal([]byte(*app.OAuthConfig), &cfg); err != nil {
		return appID, fmt.Errorf("invalid oauth config")
	}

	bag, _ := loadAppSecrets(ctx, appID)
	clientSecret := ""
	if bag != nil {
		clientSecret = bag.OAuthClientSecret
	}
	if clientSecret == "" {
		return appID, fmt.Errorf("oauth client secret not configured")
	}

	tok, err := exchangeCode(ctx, cfg.TokenURL, cfg.ClientID, clientSecret, code, redirectURI)
	if err != nil {
		return appID, fmt.Errorf("token exchange failed: %w", err)
	}

	// Encrypt + persist the access/refresh tokens.
	encAccess, _ := helpers.EncryptSecret(tok.AccessToken)
	var encRefresh *string
	if tok.RefreshToken != "" {
		r, _ := helpers.EncryptSecret(tok.RefreshToken)
		encRefresh = &r
	}
	var expiresAt *time.Time
	if tok.ExpiresIn > 0 {
		t := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		expiresAt = &t
	}

	// integrationModel.UpsertIntegration binds 9 positional args in a fixed
	// order (… access_token, refresh_token, webhook_url, metadata, expires_at,
	// updated_at). The query must reference $1..$9 in that order; a shorter
	// query silently fails, which is why OAuth app "Connect" never persisted a
	// token and the app stayed "not connected".
	const upsert = `
		INSERT INTO integrations (entity_type, entity_id, provider, access_token, refresh_token, webhook_url, metadata, expires_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (entity_type, entity_id, provider)
		DO UPDATE SET access_token = EXCLUDED.access_token, refresh_token = EXCLUDED.refresh_token,
		    expires_at = EXCLUDED.expires_at, updated_at = EXCLUDED.updated_at`
	if err := integrationModel.UpsertIntegration(upsert, "app", appID, appOAuthProviderPrefix+"token",
		&encAccess, encRefresh, nil, nil, expiresAt, time.Now()); err != nil {
		return appID, fmt.Errorf("persist token: %w", err)
	}

	return appID, nil
}

type oauthToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// exchangeCode performs the OAuth2 authorization-code exchange. SSRF-guarded.
func exchangeCode(ctx context.Context, tokenURL, clientID, clientSecret, code, redirectURI string) (*oauthToken, error) {
	if _, err := helpers.ValidateOutboundURL(tokenURL, false); err != nil {
		return nil, fmt.Errorf("token_url rejected: %w", err)
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := helpers.SSRFSafeClient(false)
	client.Timeout = 15 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}

	var tok oauthToken
	if err := json.Unmarshal(body, &tok); err != nil {
		// Some providers return form-encoded tokens; try that.
		if vals, perr := url.ParseQuery(string(body)); perr == nil {
			tok.AccessToken = vals.Get("access_token")
			tok.RefreshToken = vals.Get("refresh_token")
		}
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("no access_token in response")
	}
	return &tok, nil
}

// DisconnectOAuthApp removes the stored OAuth token (keeps the app installed).
func DisconnectOAuthApp(ctx context.Context, appID uuid.UUID) error {
	const delQ = `DELETE FROM integrations WHERE entity_type = $1 AND entity_id = $2 AND provider = $3`
	return integrationModel.DeleteIntegration(delQ, "app", appID, appOAuthProviderPrefix+"token")
}
