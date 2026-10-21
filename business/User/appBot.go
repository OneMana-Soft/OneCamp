package business

import (
	"context"
	"fmt"
	"sync"

	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/google/uuid"
)

// An installed app's principal (platform P-API, §3.1): resolved, and
// provisioned on first use, like an agent's (EnsureAgentBot). Its tokens act as
// it, and what it writes is its own.

var (
	appBotMu    sync.Mutex
	appBotCache = map[uuid.UUID]appBotCacheEntry{}
)

type appBotCacheEntry struct {
	identity *BotIdentity
	name     string
	icon     string
}

// EnsureAppBot returns the app's bot principal, provisioning it on first use
// and following the app's name and icon when they change.
func EnsureAppBot(ctx context.Context, appID uuid.UUID, displayName, iconKey string) (*BotIdentity, error) {
	appBotMu.Lock()
	defer appBotMu.Unlock()
	if e, ok := appBotCache[appID]; ok && e.name == displayName && e.icon == iconKey && e.identity.DgraphUID != "" {
		return e.identity, nil
	}
	userID, err := domain.EnsureAppBotUser(ctx, appID, displayName)
	if err != nil {
		return nil, err
	}
	id := ensureBotPrincipal(ctx, userID, domain.AppBotEmail(appID), displayNameOrFallback(displayName), iconKey)
	if id.DgraphUID == "" {
		return nil, fmt.Errorf("the app's principal has no graph node")
	}
	appBotCache[appID] = appBotCacheEntry{identity: id, name: displayName, icon: iconKey}
	return id, nil
}
