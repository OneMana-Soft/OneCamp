package business

// GitHub OAuth token lifecycle.
//
// Why this file exists
// --------------------
// The original implementation stored the access_token + refresh_token in
// the `integrations` table at OAuth callback and then read access_token
// directly from there for every subsequent API call. GitHub OAuth App
// User-to-Server tokens expire in 8 hours by default. Once expired,
// every API call (sync, webhook re-fetch, repo listing, branch
// creation, ...) returns 401 silently and stops working until an admin
// notices and re-clicks "Connect GitHub". That's a daily silent outage
// in production.
//
// What this file does
// -------------------
// Wraps GitHub's OAuth refresh in golang.org/x/oauth2's TokenSource.
// On each call, the library checks expiry, hits GitHub's token endpoint
// when needed, and returns a fresh token. We persist the refreshed
// token back to Postgres so other goroutines / pods see it.
//
// Refresh is reactive (lazy) and shared: a single in-memory TokenSource
// per process is reused, so two concurrent callers won't double-refresh.
// The library serialises refreshes internally.
//
// Callers shape
// -------------
//   client, err := business.GitHubHTTPClient(ctx)
//   if err != nil { return err }
//   resp, err := client.Get("https://api.github.com/...")
//
// The returned *http.Client transparently sets the Authorization
// header. Callers must NOT also set "Bearer ..." themselves.
//
// On disconnect / re-connect we invalidate the cached source so the
// next call rebuilds it from the new token.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	integrationDomain "github.com/akashc777/OneCamp/domain/Integration"
	"github.com/akashc777/OneCamp/helpers"
	integrationModel "github.com/akashc777/OneCamp/models/postgres/Integration"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	githubOauth "golang.org/x/oauth2/github"
)

// ErrGitHubNotConnected is returned when no GitHub integration row
// exists or the row has no access token. Callers should bubble this
// up as a 4xx so the FE can prompt re-connect.
var ErrGitHubNotConnected = errors.New("GitHub not connected")

// httpRequestTimeout is the per-call ceiling for the underlying
// transport. The oauth2 library wraps the transport, so this also
// bounds the token-refresh request.
const httpRequestTimeout = 15 * time.Second

// persistingGitHubTokenSource wraps an oauth2.TokenSource and writes
// the refreshed token back to integrationDomain whenever it changes.
//
// A single instance is reused per process via the sources cache below.
// oauth2.ReuseTokenSource (which we wrap with this) serialises refreshes
// and only calls the underlying source when the cached token is near
// expiry, so this is concurrency-safe without extra locks on Token().
type persistingGitHubTokenSource struct {
	base       oauth2.TokenSource
	last       *oauth2.Token
	persistMu  sync.Mutex
	persistCtx context.Context
}

func (s *persistingGitHubTokenSource) Token() (*oauth2.Token, error) {
	t, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	// Persist only when the token actually rotated. Comparing the
	// access string handles both the first-call seed and subsequent
	// refreshes uniformly.
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	if s.last == nil || s.last.AccessToken != t.AccessToken {
		s.last = t
		var refresh *string
		if t.RefreshToken != "" {
			refresh = &t.RefreshToken
		}
		var expiry *time.Time
		if !t.Expiry.IsZero() {
			e := t.Expiry
			expiry = &e
		}
		// Best-effort. If persistence fails the in-memory token is
		// still valid for this process, so refresh hasn't broken the
		// caller — but other pods will eventually re-refresh on their
		// own.
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := integrationDomain.UpdateIntegrationToken(bg,
			integrationModel.IntegrationEntityOrg, uuid.Nil,
			integrationModel.ProviderGitHub,
			&t.AccessToken, refresh, expiry); err != nil {
			helpers.LogErrorWithContext(s.persistCtx,
				"business/GitHub Failed to persist refreshed token: %v", err)
		} else {
			helpers.MessageLogs.InfoLog.Println("Refreshed and persisted GitHub OAuth token")
		}
	}
	return t, nil
}

// sourceCacheEntry pins the active TokenSource and the access token
// it was built from, so a re-connect (which writes a brand new token
// row) invalidates the cache and forces a rebuild.
type sourceCacheEntry struct {
	seedAccess string
	source     oauth2.TokenSource
}

var (
	sourceCacheMu sync.RWMutex
	sourceCache   *sourceCacheEntry
)

// invalidateGitHubTokenSourceCache drops the cached source. Called from
// ExchangeCodeAndSave and DisconnectGitHub so the next caller rebuilds
// from the freshly-stored row.
func invalidateGitHubTokenSourceCache() {
	sourceCacheMu.Lock()
	sourceCache = nil
	sourceCacheMu.Unlock()
}

// loadGitHubOAuthConfig reads client credentials via the admin-managed config
// layer (DB-first, ENV-fallback). Built lazily so unit tests that set neither
// the DB nor env vars can still import this package.
func loadGitHubOAuthConfig(ctx context.Context) *oauth2.Config {
	appCfg := GetGitHubAppConfig(ctx)
	return &oauth2.Config{
		ClientID:     appCfg.ClientID,
		ClientSecret: appCfg.ClientSecret,
		Endpoint:     githubOauth.Endpoint,
		Scopes:       []string{"repo", "read:org"},
	}
}

// getOrBuildTokenSource returns a process-wide TokenSource for the
// given integration. The source is cached and shared; a re-connect
// (different access token in the row) forces a rebuild.
func getOrBuildTokenSource(ctx context.Context, integration *integrationModel.Integration) oauth2.TokenSource {
	access := ""
	if integration.AccessToken != nil {
		access = *integration.AccessToken
	}

	// Fast path: cached source matches the row.
	sourceCacheMu.RLock()
	cached := sourceCache
	sourceCacheMu.RUnlock()
	if cached != nil && cached.seedAccess == access {
		return cached.source
	}

	// Build a fresh source under the write lock. Re-check inside the
	// lock so a racing builder doesn't waste work.
	sourceCacheMu.Lock()
	defer sourceCacheMu.Unlock()
	if sourceCache != nil && sourceCache.seedAccess == access {
		return sourceCache.source
	}

	cfg := loadGitHubOAuthConfig(ctx)
	seed := &oauth2.Token{AccessToken: access}
	if integration.RefreshToken != nil {
		seed.RefreshToken = *integration.RefreshToken
	}
	if integration.ExpiresAt != nil {
		seed.Expiry = *integration.ExpiresAt
	}

	// Tokens without refresh tokens (legacy "Personal Access Token"
	// or pre-refresh-feature OAuth Apps) cannot refresh. We still
	// hand out a static source so callers don't break — they'll fail
	// 401 when the token expires, same as before, but new connections
	// will benefit immediately.
	var src oauth2.TokenSource
	if seed.RefreshToken == "" {
		src = oauth2.StaticTokenSource(seed)
	} else {
		base := cfg.TokenSource(ctx, seed)
		src = oauth2.ReuseTokenSource(seed, &persistingGitHubTokenSource{
			base:       base,
			last:       seed,
			persistCtx: context.Background(),
		})
	}

	sourceCache = &sourceCacheEntry{seedAccess: access, source: src}
	return src
}

// GitHubHTTPClient returns an *http.Client that transparently injects
// a (possibly refreshed) Bearer token on every request. Returns
// ErrGitHubNotConnected if the integration row is missing or has no
// access token.
//
// The returned client has a 15s timeout matching the legacy
// githubHTTPClient. It is safe to share across goroutines.
func GitHubHTTPClient(ctx context.Context) (*http.Client, error) {
	integration, err := integrationDomain.GetIntegration(ctx,
		integrationModel.IntegrationEntityOrg, uuid.Nil,
		integrationModel.ProviderGitHub)
	if err != nil {
		return nil, fmt.Errorf("load github integration: %w", err)
	}
	if integration == nil || integration.AccessToken == nil || *integration.AccessToken == "" {
		return nil, ErrGitHubNotConnected
	}

	src := getOrBuildTokenSource(ctx, integration)

	// oauth2.NewClient wraps the transport so each request gets a
	// fresh "Authorization: Bearer ..." header. We override the
	// Timeout because oauth2.NewClient's default is no timeout, which
	// is unsafe for a public-facing IDP API.
	client := oauth2.NewClient(ctx, src)
	client.Timeout = httpRequestTimeout
	return client, nil
}

// GitHubAccessToken returns the current (refreshed-if-needed) raw
// access token. Use this only when you genuinely need the string
// (signing webhook events, building Authorization headers for code
// paths that can't take an *http.Client). Prefer GitHubHTTPClient.
func GitHubAccessToken(ctx context.Context) (string, error) {
	integration, err := integrationDomain.GetIntegration(ctx,
		integrationModel.IntegrationEntityOrg, uuid.Nil,
		integrationModel.ProviderGitHub)
	if err != nil {
		return "", fmt.Errorf("load github integration: %w", err)
	}
	if integration == nil || integration.AccessToken == nil || *integration.AccessToken == "" {
		return "", ErrGitHubNotConnected
	}
	src := getOrBuildTokenSource(ctx, integration)
	t, err := src.Token()
	if err != nil {
		return "", fmt.Errorf("github token: %w", err)
	}
	return t.AccessToken, nil
}

// LoadGitHubIntegration is a one-line shorthand for the
// (entity_type=org, entity_id=nil, provider=github) lookup that's
// repeated across the codebase. Returns ErrGitHubNotConnected when
// the row is missing or has no access token, matching the semantics
// of GitHubHTTPClient. This is the single canonical place to extend
// if we ever shard the integration row by entity_id.
func LoadGitHubIntegration(ctx context.Context) (*integrationModel.Integration, error) {
	integration, err := integrationDomain.GetIntegration(ctx,
		integrationModel.IntegrationEntityOrg, uuid.Nil,
		integrationModel.ProviderGitHub)
	if err != nil {
		return nil, fmt.Errorf("load github integration: %w", err)
	}
	if integration == nil || integration.AccessToken == nil || *integration.AccessToken == "" {
		return nil, ErrGitHubNotConnected
	}
	return integration, nil
}
