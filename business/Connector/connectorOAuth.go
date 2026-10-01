package business

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/oauth"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

// connectorOAuth.go — the per-user OAuth2 authorization-code flow.
//
// The state nonce binds the authorization to the initiating user AND provider
// ("<userUUID>:<provider>"), stored one-shot in Redis (GETDEL on consume). The
// callback can therefore only ever persist a token for the user who started
// the flow — the cornerstone of per-user isolation.

// clientCreds resolves the OAuth client id/secret a provider borrows.
func clientCreds(p Provider) (string, string, error) {
	switch p.reuseClient {
	case "google":
		id, secret := oauth.GoogleClientCreds()
		if id == "" || secret == "" {
			return "", "", fmt.Errorf("google oauth client is not configured")
		}
		return id, secret, nil
	case "github":
		id, secret := oauth.GithubClientCreds()
		if id == "" || secret == "" {
			return "", "", fmt.Errorf("github oauth client is not configured")
		}
		return id, secret, nil
	default:
		return "", "", fmt.Errorf("connector %q has no client configured", p.ID)
	}
}

// RedirectURI is the single callback the providers redirect back to. It points
// at the backend directly (mirrors the calendar/app flows) to avoid
// cross-domain cookie problems mid-handshake.
func RedirectURI() string {
	host := os.Getenv("BACKEND_DOMAIN")
	if host == "" {
		return "http://localhost:3000/connector/oauth/callback"
	}
	return oauth.BackendProtocol(host) + host + "/connector/oauth/callback"
}

// BuildAuthURL returns the provider authorize URL for a user to connect.
func BuildAuthURL(ctx context.Context, userUUID uuid.UUID, providerID string) (string, error) {
	p, ok := providerRegistry[providerID]
	if !ok {
		return "", fmt.Errorf("unknown connector: %s", providerID)
	}
	clientID, _, err := clientCreds(p)
	if err != nil {
		return "", err
	}

	// One-shot state nonce bound to user+provider.
	state := randHex(16)
	_ = redisStore.SetString(ctx, registry.ConnectorOAuthState, []string{state},
		userUUID.String()+":"+providerID)

	u, err := url.Parse(p.AuthURL)
	if err != nil {
		return "", fmt.Errorf("invalid auth url: %w", err)
	}
	q := u.Query()
	q.Set("client_id", clientID)
	q.Set("redirect_uri", RedirectURI())
	q.Set("response_type", "code")
	q.Set("state", state)
	if len(p.Scopes) > 0 {
		q.Set("scope", strings.Join(p.Scopes, " "))
	}
	// Google needs offline + consent to return a refresh token reliably.
	if p.reuseClient == "google" {
		q.Set("access_type", "offline")
		q.Set("prompt", "consent")
		q.Set("include_granted_scopes", "true")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// HandleCallback exchanges the code and persists the token for the user the
// state nonce is bound to. Returns (userUUID, providerID) for the redirect.
func HandleCallback(ctx context.Context, state, code string) (uuid.UUID, string, error) {
	if state == "" || code == "" {
		return uuid.Nil, "", fmt.Errorf("missing state or code")
	}
	bound, found, _ := redisStore.GetDelString(ctx, registry.ConnectorOAuthState, []string{state})
	if !found || bound == "" {
		return uuid.Nil, "", fmt.Errorf("invalid or expired state")
	}
	parts := strings.SplitN(bound, ":", 2)
	if len(parts) != 2 {
		return uuid.Nil, "", fmt.Errorf("malformed state binding")
	}
	userUUID, err := uuid.Parse(parts[0])
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("invalid user in state")
	}
	providerID := parts[1]
	p, ok := providerRegistry[providerID]
	if !ok {
		return userUUID, providerID, fmt.Errorf("unknown connector: %s", providerID)
	}
	clientID, clientSecret, err := clientCreds(p)
	if err != nil {
		return userUUID, providerID, err
	}

	tok, err := exchangeCode(ctx, p.TokenURL, clientID, clientSecret, code)
	if err != nil {
		return userUUID, providerID, fmt.Errorf("token exchange failed: %w", err)
	}

	st := storedToken{AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken}
	if tok.ExpiresIn > 0 {
		t := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		st.ExpiresAt = &t
	}
	if err := saveUserToken(ctx, userUUID, providerID, st); err != nil {
		return userUUID, providerID, fmt.Errorf("persist token: %w", err)
	}
	// Bust the briefing "Your day" cache so the newly-connected account shows
	// up immediately rather than after the TTL.
	invalidateBriefingDayCache(ctx, userUUID)
	return userUUID, providerID, nil
}

// validAccessToken returns a non-expired access token for the user+provider,
// transparently refreshing it when a refresh token is available. Returns
// ("", nil) when the user isn't connected.
func validAccessToken(ctx context.Context, userUUID uuid.UUID, providerID string) (string, error) {
	tok, err := loadUserToken(ctx, userUUID, providerID)
	if err != nil {
		return "", err
	}
	if tok == nil {
		return "", nil
	}
	// Still valid (with a 60s safety margin)?
	if tok.ExpiresAt == nil || time.Now().Add(60*time.Second).Before(*tok.ExpiresAt) {
		return tok.AccessToken, nil
	}
	// Expired — refresh if we can.
	if tok.RefreshToken == "" {
		return tok.AccessToken, nil // no refresh token; return as-is (call may 401 → user reconnects)
	}
	p, ok := providerRegistry[providerID]
	if !ok {
		return tok.AccessToken, nil
	}
	clientID, clientSecret, err := clientCreds(p)
	if err != nil {
		return tok.AccessToken, nil
	}
	refreshed, err := refreshToken(ctx, p.TokenURL, clientID, clientSecret, tok.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("refresh token: %w", err)
	}
	newTok := storedToken{
		AccessToken:  refreshed.AccessToken,
		RefreshToken: tok.RefreshToken, // providers often omit a new refresh token
	}
	if refreshed.RefreshToken != "" {
		newTok.RefreshToken = refreshed.RefreshToken
	}
	if refreshed.ExpiresIn > 0 {
		t := time.Now().Add(time.Duration(refreshed.ExpiresIn) * time.Second)
		newTok.ExpiresAt = &t
	}
	if err := saveUserToken(ctx, userUUID, providerID, newTok); err != nil {
		helpers.LogErrorWithContext(ctx, "connector/validAccessToken persist refreshed token err: %+v", err)
	}
	return newTok.AccessToken, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

func exchangeCode(ctx context.Context, tokenURL, clientID, clientSecret, code string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("redirect_uri", RedirectURI())
	return postToken(ctx, tokenURL, form)
}

func refreshToken(ctx context.Context, tokenURL, clientID, clientSecret, refresh string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	return postToken(ctx, tokenURL, form)
}

// postToken performs an SSRF-guarded token endpoint POST and parses JSON or
// form-encoded responses (GitHub returns form-encoded by default).
func postToken(ctx context.Context, tokenURL string, form url.Values) (*tokenResponse, error) {
	if _, err := helpers.ValidateOutboundURL(tokenURL, false); err != nil {
		return nil, fmt.Errorf("token url rejected: %w", err)
	}
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

	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
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

// revokeToken makes a best-effort token revocation call.
func revokeToken(ctx context.Context, p Provider, accessToken string) {
	if p.RevokeURL == "" || accessToken == "" {
		return
	}
	form := url.Values{}
	form.Set("token", accessToken)
	if _, err := helpers.ValidateOutboundURL(p.RevokeURL, false); err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.RevokeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := helpers.SSRFSafeClient(false)
	client.Timeout = 10 * time.Second
	if resp, derr := client.Do(req); derr == nil {
		_ = resp.Body.Close()
	}
}

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return uuid.NewString() + uuid.NewString()
	}
	return hex.EncodeToString(buf)
}
