package business

import (
	"context"

	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

// AuditAccessDue reports whether a guest opening a shared resource through a
// link is written to the audit log now: once per link and guest name per half
// hour. A guest's channel and project pages refresh every few seconds, and
// auditing every refresh wrote about 720 rows an hour for one open tab,
// burying everything else in the security log. With Redis unavailable every
// open is recorded, as before.
func AuditAccessDue(ctx context.Context, grantID uuid.UUID, guestName string) bool {
	return redisStore.AllowFixedWindow(ctx, registry.GuestAccessAudit, []string{grantID.String(), guestName}, 1).Allowed
}

// notifyDue reports whether guests' words in a channel, thread or doc tell the
// team now: once per one of them per five minutes, so a client writing a
// dozen short messages wakes people once, and what follows is there when they
// look. With Redis unavailable every message notifies, as before.
func notifyDue(ctx context.Context, what, id string) bool {
	return redisStore.AllowFixedWindow(ctx, registry.GuestNotifyOnce, []string{what, id}, 1).Allowed
}
