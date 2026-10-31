package business

import (
	"context"

	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// FetchesPerMinute is how many fetches one person may make the server do a
// minute for each kind of action: previews, stored pictures, and new links'
// titles (docs F1), each counted on its own. A cached preview costs nothing.
const FetchesPerMinute = 30

// AllowFetch counts one fetch of action for the person in their minute's
// window, in Redis or, while Redis can't be asked, in this process (the
// limit still holds), and says whether it may go ahead and, if not, in how
// many seconds it may. Unfurl and StoreRemote ask it through their budget on
// a cache miss.
func AllowFetch(ctx context.Context, action, userUUID string) (bool, int) {
	res := redisStore.AllowFixedWindowOrLocal(ctx, registry.UnfurlRate, []string{action, userUUID}, FetchesPerMinute)
	return res.Allowed, res.RetryAfterSeconds()
}
