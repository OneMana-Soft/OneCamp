package business

// Shared automation bot identity. Webhooks and workflows post AS this user so
// automated messages carry a real, recognizable identity (own name + avatar)
// and a single audit principal, instead of silently appearing as the human
// who configured the integration.
//
// The bot is a real user row (Postgres + Dgraph) flagged is_bot, but it can
// never authenticate: it has no password and is excluded from login/SSO paths.
// It mirrors the external/ghost-user dual-write pattern already used for
// unmapped GitHub/Slack users.

import (
	"context"
	"sync"
	"time"

	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// BotIdentity is the resolved, cached identity of the system automation bot:
// the postgres user id (post owner) and the Dgraph UID (graph author node).
type BotIdentity struct {
	UserID     uuid.UUID // postgres users.id — used as the post's created_by
	DgraphUID  string    // Dgraph node uid — used as PostBy
	UUID       string    // user uuid string (== UserID.String())
	Name       string    // display name ("OneCamp AI")
	ProfileKey string    // user_profile_object_key (avatar); empty until set
}

var (
	botMu       sync.RWMutex
	botIdentity *BotIdentity
)

// EnsureAutomationBot seeds (if needed) and caches the shared automation bot.
// Idempotent; call once at startup. Safe to call again — it converges on the
// single sentinel row. Returns the resolved identity.
func EnsureAutomationBot(ctx context.Context) (*BotIdentity, error) {
	// Fast path: already resolved this process.
	botMu.RLock()
	if botIdentity != nil {
		id := botIdentity
		botMu.RUnlock()
		return id, nil
	}
	botMu.RUnlock()

	// 1. Postgres row (idempotent insert).
	userID, err := domain.EnsureSystemBotUser(ctx)
	if err != nil {
		return nil, err
	}

	// 2. Dgraph node (dual-write, mirroring external users). CreateOrUpdate is
	//    keyed on user_uuid via the upsert query, so this is idempotent too.
	zeroUnixTime := time.Time{}
	dgraphUser := &dgraphStruct.DgraphUser{
		Uuid:    userID.String(),
		EmailID: domain.SystemBotEmail,
		// The display name in BOTH fields, as agent bots do (agentBot.go): the
		// Dgraph user_name is what DM lists, task cards, reactions and the search
		// index show, and the handle "onecamp-ai" there made the bot appear as a
		// login rather than a name. The unique handle stays on the Postgres
		// users.username column, so addressing is unaffected. Re-upserted on every
		// boot, so existing installs pick this up on their next restart.
		UserName:     domain.SystemBotDisplayName(),
		UserFullName: domain.SystemBotDisplayName(),
		Status:       dgraphStruct.USER_OPT_STATUS_OFFLINE,
		DeletedAt:    &zeroUnixTime,
		IsBot:        true,
		// Excluded from member pickers/lists like external users: the bot is a
		// synthetic non-member principal, the same exclusion class as ghosts.
		IsExternal: true,
	}
	dgraphUID, derr := CreateOrUpdateDgraphUser(ctx, dgraphUser)
	if derr != nil {
		helpers.LogErrorWithContext(ctx, "business/EnsureAutomationBot dgraph write failed: %+v", derr)
	}

	// Resolve the canonical node to capture the Dgraph UID and the avatar
	// (user_profile_object_key → DgraphUser.ProfileKey). The upsert above may
	// not return the UID when the node already existed, and the profile key is
	// only known via a read, so always reconcile from a fresh read.
	var profileKey string
	if existing, rerr := domain.GetActiveDgraphUserInfoByUUID(ctx, userID.String()); rerr == nil && existing != nil {
		if existing.Uid != "" {
			dgraphUID = existing.Uid
		}
		if existing.ProfileKey != nil {
			profileKey = *existing.ProfileKey
		}
	}

	id := &BotIdentity{
		UserID:     userID,
		DgraphUID:  dgraphUID,
		UUID:       userID.String(),
		Name:       domain.SystemBotDisplayName(),
		ProfileKey: profileKey,
	}

	botMu.Lock()
	botIdentity = id
	botMu.Unlock()

	helpers.MessageLogs.InfoLog.Printf("Automation bot ready (user=%s dgraph=%s)", id.UUID, id.DgraphUID)
	return id, nil
}

// GetAutomationBot returns the cached bot identity, resolving it on demand if
// the process hasn't seeded it yet. Returns nil only on a hard failure.
func GetAutomationBot(ctx context.Context) *BotIdentity {
	botMu.RLock()
	if botIdentity != nil {
		id := botIdentity
		botMu.RUnlock()
		return id
	}
	botMu.RUnlock()

	id, err := EnsureAutomationBot(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetAutomationBot resolve failed: %+v", err)
		return nil
	}
	return id
}
