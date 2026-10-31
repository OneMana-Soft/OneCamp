// Package business (App) is App Identity (#35; platform P-API,
// design/platform/API_CONTRACT.md §3.1, signed 17 Oct 2026): an installed app
// (business/Command's apps) as an identity of its own. An app has:
//
//   - a bot user, which its credentials act as and its messages are written by
//     (business/User EnsureAppBot), so what it does reads as the app's;
//   - scopes, the most any of its credentials may carry;
//   - an audience, the places it was added to, which every /v1 route holds it
//     to (business/PublicAPI): a route that names what it reaches (Reach)
//     refuses it anywhere else, a list or a search is cut to them (Narrow),
//     and any other route refuses an app;
//   - credentials, API tokens bound to it ("oc_app_…"), rotated in place.
//
// R1: an app's call is an API token's call. The token mark is set, so
// helpers.ActsThroughToken holds and every admin-power check refuses it; this
// package only narrows what an app may do (ActsAsApp, MayReach), never widens it.
// Turning the app off, or removing it, stops every credential on the next call
// (apiTokenBusiness.Validate asks the app's record).
package business

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	tokenModel "github.com/akashc777/OneCamp/models/postgres/ApiToken"
	appModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
	"github.com/google/uuid"
)

// How long an app's credential lives unless asked otherwise (§6): 90 days,
// rotated in place before then.
const DefaultTokenLife = 90 * 24 * time.Hour

// MaxTokensPerApp is how many live credentials an app may hold at once: one in
// use and one being rotated in, with room for a second client.
const MaxTokensPerApp = 3

// ErrNotFound is an app that doesn't exist or was removed.
var ErrNotFound = errors.New("there's no such app")

// ErrStale is a change made from an access version the app no longer has:
// someone changed what it may do since it was read.
var ErrStale = errors.New("the app's access changed since it was read")

// Place is somewhere an app was added: a channel, a project, a table.
type Place struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Audience is where an app may read and write.
type Audience struct {
	Places []Place `json:"places"`
	// Public: the public places of a kind whose rule allows it (public
	// channels), when a scope lets the app read them. Never a DM or group.
	Public bool `json:"public"`
}

// Identity is an installed app as an identity.
type Identity struct {
	AppID     uuid.UUID  `json:"app_id"`
	Slug      string     `json:"slug"`
	Name      string     `json:"name"`
	IconURL   string     `json:"icon_url,omitempty"`
	Enabled   bool       `json:"enabled"`
	BotUserID *uuid.UUID `json:"bot_user_id,omitempty"`
	Scopes    []string   `json:"scopes"`
	Audience  Audience   `json:"audience"`
	// AccessVersion is the version of Scopes and Audience read here, which a
	// change is made from (SetAccess).
	AccessVersion string `json:"access_version"`
}

// Get reads an app's identity, or ErrNotFound.
func Get(ctx context.Context, appID uuid.UUID) (*Identity, error) {
	row, err := appModel.GetAppIdentity(ctx, appID)
	if err != nil {
		return nil, err
	}
	if row == nil || row.Deleted {
		return nil, ErrNotFound
	}
	id := &Identity{AppID: row.Id, Slug: row.Slug, Name: row.Name, Enabled: row.Enabled, BotUserID: row.BotUserId, AccessVersion: row.AccessVersion}
	if row.IconUrl != nil {
		id.IconURL = *row.IconUrl
	}
	_ = json.Unmarshal([]byte(row.Scopes), &id.Scopes)
	_ = json.Unmarshal([]byte(row.Audience), &id.Audience)
	if id.Scopes == nil {
		id.Scopes = []string{}
	}
	if id.Audience.Places == nil {
		id.Audience.Places = []Place{}
	}
	return id, nil
}

// SetAccess sets what an app's credentials may carry and where it may reach.
// Credentials already made keep their scopes only as far as these allow
// (MayCarry, asked per call by ActsAsApp's callers through the token's scopes).
// from is the AccessVersion the change was made from: once someone has changed
// the app since, nothing changes and the answer is ErrStale, so of two admins
// changing it from one read, the second doesn't silently undo the first. ""
// changes it whatever it is.
func SetAccess(ctx context.Context, appID uuid.UUID, scopes []string, audience Audience, from string) (*Identity, error) {
	if _, err := Get(ctx, appID); err != nil {
		return nil, err
	}
	clean, err := knownScopes(scopes)
	if err != nil {
		return nil, err
	}
	places, err := cleanPlaces(audience.Places)
	if err != nil {
		return nil, err
	}
	audience.Places = places
	sj, _ := json.Marshal(clean)
	aj, _ := json.Marshal(audience)
	if err := appModel.SetAppAccess(ctx, appID, string(sj), string(aj), from); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		// Nothing changed: the app went, or its access moved on from `from`.
		if _, err := Get(ctx, appID); err != nil {
			return nil, err
		}
		return nil, ErrStale
	}
	return Get(ctx, appID)
}

// knownScopes de-duplicates and sorts scopes, refusing one no token could carry.
func knownScopes(scopes []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		if !apiTokenBusiness.ValidScopes([]string{s}) {
			return nil, fmt.Errorf("unknown scope: %s", s)
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// cleanPlaces refuses a place with no kind, or an id that isn't one.
func cleanPlaces(places []Place) ([]Place, error) {
	out := []Place{}
	seen := map[Place]bool{}
	for _, p := range places {
		p.Kind = strings.TrimSpace(p.Kind)
		p.ID = strings.TrimSpace(p.ID)
		if p.Kind == "" || p.ID == "" {
			return nil, fmt.Errorf("a place needs a kind and an id")
		}
		if _, err := uuid.Parse(p.ID); err != nil {
			return nil, fmt.Errorf("%q isn't a %s's id", p.ID, p.Kind)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// MayCarry is whether scope is within the app's own scopes.
func (a *Identity) MayCarry(scope string) bool {
	for _, s := range a.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// MintToken makes a credential for an app, acting as its bot user (provisioned
// here on first use), with scopes within the app's own. The plaintext is
// returned once. ttl <= 0 is DefaultTokenLife.
func MintToken(ctx context.Context, appID uuid.UUID, name string, scopes []string, ttl time.Duration) (*apiTokenBusiness.CreatedToken, error) {
	app, err := Get(ctx, appID)
	if err != nil {
		return nil, err
	}
	if !app.Enabled {
		return nil, fmt.Errorf("%s is turned off; turn it on before making it a token", app.Name)
	}
	for _, s := range scopes {
		if !app.MayCarry(strings.TrimSpace(s)) {
			return nil, fmt.Errorf("%s may not use %s; add it to the app's scopes first", app.Name, s)
		}
	}
	live, err := tokenModel.ListByApp(ctx, appID)
	if err != nil {
		return nil, err
	}
	if len(live) >= MaxTokensPerApp {
		return nil, fmt.Errorf("%s already has %d tokens; revoke one it no longer uses", app.Name, len(live))
	}
	bot, err := botOf(ctx, app)
	if err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = DefaultTokenLife
	}
	expires := time.Now().Add(ttl)
	return apiTokenBusiness.MintAppToken(ctx, name, scopes, &expires, bot, appID)
}

// botOf is the app's bot user, provisioned on first use and recorded on the app.
func botOf(ctx context.Context, app *Identity) (uuid.UUID, error) {
	bot, err := userBusiness.EnsureAppBot(ctx, app.AppID, app.Name, app.IconURL)
	if err != nil {
		return uuid.Nil, fmt.Errorf("couldn't make %s its user: %w", app.Name, err)
	}
	if app.BotUserID == nil || *app.BotUserID != bot.UserID {
		if err := appModel.SetAppBotUser(ctx, app.AppID, bot.UserID); err != nil {
			return uuid.Nil, err
		}
	}
	return bot.UserID, nil
}

// Tokens are an app's live credentials, newest first (no secrets).
func Tokens(ctx context.Context, appID uuid.UUID) ([]*tokenModel.ApiToken, error) {
	if _, err := Get(ctx, appID); err != nil {
		return nil, err
	}
	return tokenModel.ListByApp(ctx, appID)
}

// RotateToken gives one of the app's credentials a new secret and a fresh life,
// keeping its id, name and scopes; the old secret stops working at once.
func RotateToken(ctx context.Context, appID, tokenID uuid.UUID) (string, error) {
	if err := ownsToken(ctx, appID, tokenID); err != nil {
		return "", err
	}
	return apiTokenBusiness.RotateAppSecret(ctx, tokenID, time.Now().Add(DefaultTokenLife))
}

// RevokeToken stops one of the app's credentials.
func RevokeToken(ctx context.Context, appID, tokenID uuid.UUID) error {
	if err := ownsToken(ctx, appID, tokenID); err != nil {
		return err
	}
	_, err := tokenModel.RevokeAny(ctx, tokenID)
	return err
}

// ownsToken refuses a credential that isn't one of this app's.
func ownsToken(ctx context.Context, appID, tokenID uuid.UUID) error {
	live, err := Tokens(ctx, appID)
	if err != nil {
		return err
	}
	for _, t := range live {
		if t.Id == tokenID {
			return nil
		}
	}
	return fmt.Errorf("that token isn't one of this app's, or it was already revoked")
}

// ActsAsApp is the app a request's credential belongs to, from the context the
// token middleware made (helpers.ApiAppIDContextKey, set beside the token mark),
// or nil when the credential isn't an app's. It is an error when the app's
// record can't be read, or the app was turned off or removed since its token
// was checked: the caller refuses, so a failed read never opens anything.
func ActsAsApp(ctx context.Context) (*Identity, error) {
	raw := helpers.GetApiAppID(ctx)
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("the token's app %q: %w", raw, err)
	}
	app, err := Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !app.Enabled {
		return nil, fmt.Errorf("%s is turned off", app.Name)
	}
	return app, nil
}

// Kinds whose owners add rules beyond the places an app was added to (a public
// channel by scope, never a DM it isn't in) register them here when their first
// rule comes (chat item 12): RegisterAudienceRule, as API_CONTRACT.md §3.1 has it.
// Until then an app reaches only where it was added.

// MayReach is whether an app may read or write a resource: a place it was
// added to. Nothing is open by default.
func MayReach(_ context.Context, app *Identity, kind, id string) bool {
	if app == nil {
		return false
	}
	for _, p := range app.Audience.Places {
		if p.Kind == kind && p.ID == id {
			return true
		}
	}
	return false
}

// Narrow is what of items an app may reach, as a list or a search answers it:
// an item is kept when one of the places it sits in (placesOf: a task's own,
// or its project) is one the app may reach. An item in none is left out.
func Narrow[T any](ctx context.Context, app *Identity, items []T, placesOf func(T) []Place) []T {
	kept := make([]T, 0, len(items))
	for _, item := range items {
		for _, p := range placesOf(item) {
			if MayReach(ctx, app, p.Kind, p.ID) {
				kept = append(kept, item)
				break
			}
		}
	}
	return kept
}
