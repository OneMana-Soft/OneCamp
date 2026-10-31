package business

// Per-agent bot principal (resolution + cache). Each Agent Builder agent has
// its own workspace principal so it authors messages AS ITSELF (its own name,
// avatar and uuid), instead of sharing the single automation bot identity with
// a per-message display-name override.
//
// Resolution is by the deterministic sentinel email (domain.AgentBotEmail), so
// it is idempotent and does NOT depend on the denormalized ai_agents.bot_user_id
// column — that column is a convenience the caller may persist for membership /
// cleanup. The resolved identity is cached per agent id (with its name + avatar
// reconciled on change) so a hot mention path does not re-read on every reply.

import (
	"context"
	"fmt"
	"sync"
	"time"

	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// agentBotCacheEntry caches a resolved agent principal plus the name/avatar it
// was last reconciled against, so a later resolve only re-writes when the
// agent's display identity actually changed.
type agentBotCacheEntry struct {
	identity *BotIdentity
	name     string
	avatar   string
}

var (
	agentBotMu    sync.RWMutex
	agentBotCache = map[uuid.UUID]*agentBotCacheEntry{}
)

// EnsureAgentBot resolves (provisioning on first use) the bot principal for an
// agent and returns its identity. displayName is the agent's name; avatarKey is
// the agent's avatar object key (may be empty → the FE renders initials).
//
// Idempotent and concurrency-safe. The returned identity's UserID can be
// persisted to ai_agents.bot_user_id by the caller, but resolution never
// depends on that column.
func EnsureAgentBot(ctx context.Context, agentID uuid.UUID, displayName, avatarKey string) (*BotIdentity, error) {
	// Fast path: cached and the display identity is unchanged.
	agentBotMu.RLock()
	if e, ok := agentBotCache[agentID]; ok && e.name == displayName && e.avatar == avatarKey {
		id := e.identity
		agentBotMu.RUnlock()
		return id, nil
	}
	agentBotMu.RUnlock()

	// 1. Postgres row (idempotent; reconciles display name).
	userID, err := domain.EnsureAgentBotUser(ctx, agentID, displayName)
	if err != nil {
		return nil, err
	}
	id := ensureBotPrincipal(ctx, userID, domain.AgentBotEmail(agentID), displayNameOrFallback(displayName), avatarKey)

	agentBotMu.Lock()
	agentBotCache[agentID] = &agentBotCacheEntry{identity: id, name: displayName, avatar: avatarKey}
	agentBotMu.Unlock()

	return id, nil
}

// ensureBotPrincipal writes a bot's Dgraph node over its Postgres row and
// returns the identity messages are authored with.
func ensureBotPrincipal(ctx context.Context, userID uuid.UUID, email, name, avatarKey string) *BotIdentity {

	// 2. Dgraph node (dual-write, mirroring the shared bot + external users).
	//    Keyed on user_uuid via upsert, so idempotent. Avatar is passed through
	//    as the profile object key; if it is not a resolvable attachment the FE
	//    falls back to initials, so a bad/empty key is safe.
	zeroUnixTime := time.Time{}
	dgraphUser := &dgraphStruct.DgraphUser{
		Uuid:    userID.String(),
		EmailID: email,
		// Display name (the agent's name) is used for BOTH user_name and
		// user_full_name so every surface that renders an assignee/author by
		// user_name (the task table, kanban cards) shows "Release Captain"
		// rather than the internal "agent-bot-<uuid>" handle. The unique handle
		// stays on the Postgres users.username column (see EnsureAgentBotUser),
		// so addressing/uniqueness is unaffected; the Dgraph user_name is purely
		// a display field.
		UserName:     name,
		UserFullName: name,
		Status:       dgraphStruct.USER_OPT_STATUS_OFFLINE,
		DeletedAt:    &zeroUnixTime,
		IsBot:        true,
		IsExternal:   true,
	}
	if avatarKey != "" {
		ak := avatarKey
		dgraphUser.ProfileKey = &ak
	}
	dgraphUID, derr := CreateOrUpdateDgraphUser(ctx, dgraphUser)
	if derr != nil {
		helpers.LogErrorWithContext(ctx, "business/ensureBotPrincipal dgraph write failed (%s): %+v", email, derr)
	}

	// Reconcile the canonical node to capture the UID (upsert may not return it
	// when the node already existed) and the persisted profile key.
	profileKey := avatarKey
	if existing, rerr := domain.GetActiveDgraphUserInfoByUUID(ctx, userID.String()); rerr == nil && existing != nil {
		if existing.Uid != "" {
			dgraphUID = existing.Uid
		}
		if existing.ProfileKey != nil {
			profileKey = *existing.ProfileKey
		}
	}

	return &BotIdentity{
		UserID:     userID,
		DgraphUID:  dgraphUID,
		UUID:       userID.String(),
		Name:       name,
		ProfileKey: profileKey,
		Kind:       domain.ClassifyBot(email),
	}
}

// fixedBot is one of the workspace's fixed principals, each with a job of its
// own, so what it writes never reads as the assistant's, an agent's or a
// person's. It is provisioned on first use and kept for the process once it
// has a graph node.
type fixedBot struct {
	email, username, name string

	mu sync.Mutex
	id *BotIdentity
}

// ensure resolves the principal, provisioning it on first use.
func (b *fixedBot) ensure(ctx context.Context) (*BotIdentity, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.id != nil && b.id.DgraphUID != "" {
		return b.id, nil
	}
	userID, err := domain.EnsureBotUser(ctx, b.email, b.username, b.name)
	if err != nil {
		return nil, err
	}
	id := ensureBotPrincipal(ctx, userID, b.email, b.name, "")
	if id.DgraphUID == "" {
		return nil, fmt.Errorf("the %s principal has no graph node", b.username)
	}
	b.id = id
	return id, nil
}

var (
	// The principal that authors messages arriving from a linked Slack
	// channel, so a Slack message never reads as the AI.
	slackBridgeBot = &fixedBot{email: domain.SlackBridgeBotEmail, username: domain.SlackBridgeBotUsername, name: "Slack"}
	// The principal that writes for guests: channel messages and task
	// comments from people outside the workspace.
	channelGuestBot = &fixedBot{email: domain.ChannelGuestBotEmail, username: domain.ChannelGuestBotUsername, name: "Guests"}
	// The principal that asks channels' automatic check-ins, so a check-in
	// reads as "Check-in" after a reload.
	checkInBot = &fixedBot{email: domain.CheckInBotEmail, username: domain.CheckInBotUsername, name: "Check-in"}
	// The principal an upgrade's repairs act as, named on what they change.
	upgradeRepairBot = &fixedBot{email: domain.UpgradeRepairBotEmail, username: domain.UpgradeRepairBotUsername, name: "OneCamp, upgrade repair"}
)

// EnsureSlackBridgeBot resolves (provisioning on first use) the principal
// that authors messages arriving from a linked Slack channel.
func EnsureSlackBridgeBot(ctx context.Context) (*BotIdentity, error) {
	return slackBridgeBot.ensure(ctx)
}

// EnsureGuestBot resolves (provisioning on first use) the principal that
// writes for guests, so a guest's words never read as the AI's or a member's.
func EnsureGuestBot(ctx context.Context) (*BotIdentity, error) {
	return channelGuestBot.ensure(ctx)
}

// EnsureCheckInBot resolves (provisioning on first use) the principal that
// asks channels' automatic check-ins.
func EnsureCheckInBot(ctx context.Context) (*BotIdentity, error) {
	return checkInBot.ensure(ctx)
}

// EnsureUpgradeRepairBot resolves (provisioning on first use) the principal an
// upgrade's repairs act as: "OneCamp, upgrade repair", never a person, on a
// line in a task's history. An install with nothing to repair never gets it.
func EnsureUpgradeRepairBot(ctx context.Context) (*BotIdentity, error) {
	return upgradeRepairBot.ensure(ctx)
}

// InvalidateAgentBot drops an agent's cached principal (call after delete) so a
// stale identity is not served. Resolution will re-provision on next use.
func InvalidateAgentBot(agentID uuid.UUID) {
	agentBotMu.Lock()
	delete(agentBotCache, agentID)
	agentBotMu.Unlock()
}

func displayNameOrFallback(name string) string {
	if name == "" {
		return "AI Agent"
	}
	return name
}
