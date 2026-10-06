// Package business (Connector) is OneCamp's per-USER integration layer — the
// "connectors" that let the workspace AI read and act on a user's external
// accounts (Gmail, Google Calendar, GitHub), in the spirit of Claude/ChatGPT
// connectors.
//
// Design pillars (all enforced here, not left to call sites):
//
//   - PER-USER ISOLATION: every token is stored in the `integrations` table
//     keyed by (entity_type="user", entity_id=<the connecting user>, provider).
//     The AI tool executors resolve a client strictly from the *requesting*
//     user's UUID, so connector A's account can never be read to answer for
//     user B. This is the single most important invariant.
//
//   - ENCRYPTED AT REST: access + refresh tokens are sealed with
//     helpers.EncryptSecret (AES-256-GCM) before they touch Postgres and are
//     only decrypted in-process at call time.
//
//   - LEAST PRIVILEGE + EXPLICIT CONSENT: each provider declares the minimal
//     scope set it needs; the connect UI shows exactly what the AI will be able
//     to see/do, and disconnect revokes + deletes the token.
//
//   - READ vs WRITE: read tools run on demand; write tools (send email, comment
//     on a PR, create an event) flow through the existing AI ProposedAction
//     confirmation gate and are never auto-executed.
//
// The package depends only on the integrations model + helpers + the oauth
// credential resolver, so it has no upward import cycles.
package business

import (
	"context"
	"fmt"
	"strings"
	"time"

	integrationDomain "github.com/akashc777/OneCamp/domain/Integration"
	"github.com/akashc777/OneCamp/helpers"
	integrationModel "github.com/akashc777/OneCamp/models/postgres/Integration"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

// Provider ids for per-user connectors. These are distinct from the existing
// login/integration providers (e.g. "google_calendar" is reused; "gmail" and
// "github_connector" are new) so connector tokens never collide with the
// calendar-sync or GitHub-App integration rows.
const (
	ProviderGmail    = "gmail"
	ProviderCalendar = "google_calendar"  // shares the existing calendar integration row
	ProviderGitHub   = "github_connector" // per-user GitHub (distinct from the GitHub App)
)

// entityTypeUser is the integrations.entity_type for per-user connector tokens.
const entityTypeUser = "user"

// ErrNotConnected is returned by client operations when the user hasn't
// connected the relevant provider. Executors map this to a friendly "connect
// your account first" message.
var ErrNotConnected = fmt.Errorf("connector not connected")

// Capability classifies what a connector tool does, so the UI/consent screen
// and the confirmation gate can treat reads and writes differently.
type Capability string

const (
	CapabilityRead  Capability = "read"
	CapabilityWrite Capability = "write"
)

// ScopeInfo is a human-readable description of one permission the connector
// requests, shown on the consent screen.
type ScopeInfo struct {
	Capability  Capability `json:"capability"`
	Description string     `json:"description"`
}

// Provider describes a connectable external service.
type Provider struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	IconKey     string      `json:"icon_key"` // FE maps this to an icon
	AuthURL     string      `json:"-"`
	TokenURL    string      `json:"-"`
	RevokeURL   string      `json:"-"` // optional; best-effort token revocation
	Scopes      []string    `json:"-"`
	Permissions []ScopeInfo `json:"permissions"`
	// reuseClient names which admin OAuth client this provider borrows:
	// "google" or "github". Connectors don't need their own client creds.
	reuseClient string
}

// providerRegistry holds the known connector providers. Populated by init() in
// the providers.go file.
var providerRegistry = map[string]Provider{}

// register adds a provider to the registry. Called from provider init().
func register(p Provider) {
	providerRegistry[p.ID] = p
}

// Providers returns all registered connector providers (stable order by name).
func Providers() []Provider {
	out := make([]Provider, 0, len(providerRegistry))
	for _, p := range providerRegistry {
		out = append(out, p)
	}
	// simple insertion sort by name (small N)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && strings.ToLower(out[j].Name) < strings.ToLower(out[j-1].Name); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// --- Per-user token storage (encrypted) ---

// storedToken is the decrypted token bundle used in-process.
type storedToken struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    *time.Time
}

// saveUserToken encrypts and upserts a user's connector token. The
// (entity_type, entity_id, provider) unique key guarantees one row per
// user+provider.
func saveUserToken(ctx context.Context, userUUID uuid.UUID, provider string, tok storedToken) error {
	encAccess, err := helpers.EncryptSecret(tok.AccessToken)
	if err != nil {
		return fmt.Errorf("encrypt access token: %w", err)
	}
	var encRefresh *string
	if tok.RefreshToken != "" {
		r, rerr := helpers.EncryptSecret(tok.RefreshToken)
		if rerr != nil {
			return fmt.Errorf("encrypt refresh token: %w", rerr)
		}
		encRefresh = &r
	}
	return integrationDomain.UpsertIntegration(ctx, entityTypeUser, userUUID, provider,
		&encAccess, encRefresh, nil, nil, tok.ExpiresAt)
}

// loadUserToken fetches and decrypts a user's connector token. Returns
// (nil, nil) when the user hasn't connected this provider.
func loadUserToken(ctx context.Context, userUUID uuid.UUID, provider string) (*storedToken, error) {
	rec, err := integrationDomain.GetIntegration(ctx, entityTypeUser, userUUID, provider)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.AccessToken == nil || *rec.AccessToken == "" {
		return nil, nil
	}
	access, err := helpers.DecryptSecret(*rec.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("decrypt access token: %w (%w)", ErrCredentialUnreadable, err)
	}
	out := &storedToken{AccessToken: access, ExpiresAt: rec.ExpiresAt}
	if rec.RefreshToken != nil && *rec.RefreshToken != "" {
		refresh, derr := helpers.DecryptSecret(*rec.RefreshToken)
		if derr != nil {
			return nil, fmt.Errorf("decrypt refresh token: %w (%w)", ErrCredentialUnreadable, derr)
		}
		out.RefreshToken = refresh
	}
	return out, nil
}

// IsConnected reports whether a user has an active token for a provider.
func IsConnected(ctx context.Context, userUUID uuid.UUID, provider string) bool {
	tok, err := loadUserToken(ctx, userUUID, provider)
	return err == nil && tok != nil
}

// ConnectedProviderNames returns the human-readable names of the connectors a
// user currently has linked (e.g. ["Gmail", "Google Calendar"]). Used to make
// the AI aware of what it can actually access, so it never claims it lacks
// access to a connected account. Accepts the user UUID as a string (the form
// callers hold) and is a no-op safe on an invalid UUID. Order follows the
// provider registry's stable name order.
func ConnectedProviderNames(ctx context.Context, userUUIDStr string) []string {
	userUUID, err := uuid.Parse(userUUIDStr)
	if err != nil {
		return nil
	}
	var names []string
	for _, p := range Providers() {
		if IsConnected(ctx, userUUID, p.ID) {
			names = append(names, p.Name)
		}
	}
	return names
}

// Disconnect revokes (best-effort) and deletes a user's connector token.
func Disconnect(ctx context.Context, userUUID uuid.UUID, provider string) error {
	if p, ok := providerRegistry[provider]; ok && p.RevokeURL != "" {
		if tok, _ := loadUserToken(ctx, userUUID, provider); tok != nil {
			revokeToken(ctx, p, tok.AccessToken)
		}
	}
	const delQ = `DELETE FROM integrations WHERE entity_type = $1 AND entity_id = $2 AND provider = $3`
	err := integrationModel.DeleteIntegration(delQ, entityTypeUser, userUUID, provider)
	// Bust the briefing "Your day" cache so the removed account disappears
	// from the briefing immediately.
	invalidateBriefingDayCache(ctx, userUUID)
	return err
}

// invalidateBriefingDayCache drops the cached cross-connector "Your day"
// briefing for a user, so connect/disconnect is reflected on the next home
// load instead of after the TTL. Defined here (not in business/AI) to avoid an
// import cycle — the connector layer owns its token lifecycle and the cache
// key it must bust.
func invalidateBriefingDayCache(ctx context.Context, userUUID uuid.UUID) {
	_ = redisStore.Delete(ctx, registry.ConnectorBriefingDay, []string{userUUID.String()})
}

// GetProvider returns a provider definition by id.
func GetProvider(id string) (Provider, bool) {
	p, ok := providerRegistry[id]
	return p, ok
}
