package provider

// OAuth refresh helpers shared by import providers (Jira, Asana, Notion
// for OAuth installs).
//
// Why this lives here, not per-provider
// -------------------------------------
// Jira 3LO and Asana OAuth tokens both expire in ~1 hour. A historical
// import that takes longer than that — which is most of them — was
// silently failing as soon as the access token expired. Each provider
// has its own oauth2.Config endpoint, but the lifecycle is identical:
//   1. On every call, check if the token is expired or near expiry.
//   2. If yes, POST refresh_token to the provider's token endpoint,
//      decode {access_token, refresh_token, expires_in}.
//   3. Re-encrypt and re-save via importModels.SaveToken.
//   4. Return the fresh access_token.
//
// Centralising avoids each provider re-implementing the cache + race
// + persistence story.
//
// Concurrency: a single TokenSource per (provider, owner_user_id) is
// shared in-process via a sync.Map, with oauth2.ReuseTokenSource
// serialising refreshes inside it. Two goroutines that hit the same
// stale token will fan into one refresh round-trip.

import (
	"context"
	"fmt"
	"sync"
	"time"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

// OAuthConfigFor returns a per-provider oauth2.Config built from env
// vars. Providers register their key prefix (e.g. "JIRA",
// "ASANA") and we read XXX_OAUTH_CLIENT_ID and XXX_OAUTH_CLIENT_SECRET.
//
// The endpoint and scopes come from the caller because each provider's
// authorize / token URLs are unique.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	Endpoint     oauth2.Endpoint
}

// persistingImportTokenSource wraps an oauth2.TokenSource and writes
// the refreshed token back to import_oauth_tokens whenever it changes.
type persistingImportTokenSource struct {
	base        oauth2.TokenSource
	last        *oauth2.Token
	provider    string
	ownerUserId uuid.UUID
	persistMu   sync.Mutex
}

func (s *persistingImportTokenSource) Token() (*oauth2.Token, error) {
	t, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	if s.last == nil || s.last.AccessToken != t.AccessToken {
		s.last = t
		// Best-effort persistence. If it fails, the in-memory source
		// still serves correct tokens for the lifetime of this process;
		// other pods will re-refresh on their own.
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Load existing row to preserve fields we don't rotate (scopes,
		// source_account_id, metadata).
		existing, err := importModels.LoadToken(bg, s.provider, s.ownerUserId)
		if err != nil || existing == nil {
			return t, nil
		}
		existing.AccessToken = t.AccessToken
		if t.RefreshToken != "" {
			existing.RefreshToken = t.RefreshToken
		}
		if !t.Expiry.IsZero() {
			e := t.Expiry
			existing.ExpiresAt = &e
		}
		_ = importModels.SaveToken(bg, existing)
	}
	return t, nil
}

// importTokenSourceCacheKey identifies one cached source. Not exported
// — the surface is FreshAccessToken / TokenSource.
type importTokenSourceCacheKey struct {
	provider string
	owner    uuid.UUID
}

type importTokenSourceCacheEntry struct {
	seedAccess string
	source     oauth2.TokenSource
}

var importTokenSources sync.Map // importTokenSourceCacheKey -> *importTokenSourceCacheEntry

// InvalidateImportTokenSource drops the cached source for one
// (provider, owner) pair. Call from the OAuth callback after re-saving.
func InvalidateImportTokenSource(providerName string, owner uuid.UUID) {
	importTokenSources.Delete(importTokenSourceCacheKey{providerName, owner})
}

// FreshAccessToken returns a possibly-refreshed access token for the
// given (provider, owner). The provider's cfg supplies the client
// credentials and token endpoint. Returns the raw access token string,
// matching the existing call sites' shape.
//
// If cfg.ClientID / cfg.ClientSecret are empty (unconfigured OAuth on
// this deploy) we fall back to returning the stored access token as-is
// — that's the right behaviour for PAT / API-token connections that
// were never going to refresh anyway.
func FreshAccessToken(ctx context.Context, cfg OAuthConfig, providerName string, owner uuid.UUID) (string, error) {
	t, err := importModels.LoadToken(ctx, providerName, owner)
	if err != nil {
		return "", fmt.Errorf("load token: %w", err)
	}
	if t == nil || t.AccessToken == "" {
		return "", fmt.Errorf("no token for provider %s, owner %s", providerName, owner)
	}

	// Static-only path: PAT, API token, or unconfigured OAuth. We have
	// nothing to refresh against, so just return what's stored.
	if t.RefreshToken == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return t.AccessToken, nil
	}

	src := getOrBuildImportSource(cfg, providerName, owner, t)
	out, err := src.Token()
	if err != nil {
		// Refresh failed (revoked, network, etc.). Hand back the stale
		// token so the caller surfaces the underlying API's 401 with a
		// clear "reconnect required" message instead of an opaque
		// refresh error.
		return t.AccessToken, nil
	}
	return out.AccessToken, nil
}

func getOrBuildImportSource(cfg OAuthConfig, providerName string, owner uuid.UUID, t *importModels.Token) oauth2.TokenSource {
	key := importTokenSourceCacheKey{providerName, owner}
	if v, ok := importTokenSources.Load(key); ok {
		entry := v.(*importTokenSourceCacheEntry)
		if entry.seedAccess == t.AccessToken {
			return entry.source
		}
		// Token rotated externally (e.g. user reconnected). Drop and
		// rebuild below.
		importTokenSources.Delete(key)
	}

	seed := &oauth2.Token{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
	}
	if t.ExpiresAt != nil {
		seed.Expiry = *t.ExpiresAt
	}

	oauthCfg := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     cfg.Endpoint,
	}
	base := oauthCfg.TokenSource(context.Background(), seed)
	src := oauth2.ReuseTokenSource(seed, &persistingImportTokenSource{
		base:        base,
		last:        seed,
		provider:    providerName,
		ownerUserId: owner,
	})

	entry := &importTokenSourceCacheEntry{seedAccess: t.AccessToken, source: src}
	// LoadOrStore handles two builders racing: only one wins, the other
	// throws away its source and re-reads.
	if existing, loaded := importTokenSources.LoadOrStore(key, entry); loaded {
		return existing.(*importTokenSourceCacheEntry).source
	}
	return src
}
