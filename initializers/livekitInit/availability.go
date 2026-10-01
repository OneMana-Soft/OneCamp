package livekitInit

import (
	"context"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	livekit "github.com/livekit/protocol/livekit"
)

// Calls are an OPTIONAL subsystem, and this is what makes that true in practice.
//
// WHY THIS EXISTS. Startup treated an unreachable LiveKit as FATAL: ConnectLiveKit does
// not merely build clients, it makes a real ListRooms call, and main exited when it
// failed. Meanwhile the shipped compose file defines twelve services and none of them is
// LiveKit, while the shipped env template points LIVEKIT_HOST at http://livekit:7880.
// So the archive every customer downloads could not boot — the backend exited and, with
// `restart: unless-stopped`, restarted forever.
//
// Calling is a feature. Losing it should cost the calling feature, not the workspace.
// The server now starts without LiveKit, this reports calls as unavailable, and the
// client hides the call buttons rather than offering something that cannot work.

const (
	// reachabilityTTL bounds how often the probe touches the network. /config/client is
	// requested on page load, so an uncached probe per request would put a LiveKit
	// round-trip in front of every page.
	reachabilityTTL = 30 * time.Second
	// probeTimeout keeps one probe from holding a config request open. Deliberately
	// shorter than the 8s used at startup: this runs while a user waits for a page,
	// whereas startup can afford to be patient.
	probeTimeout = 2 * time.Second
	// cacheKey — one subsystem, one entry.
	cacheKey = "livekit-reachable"
)

// reachable caches the last probe result.
//
// TTL-cached rather than a boolean set once at startup, and that matters for a case the
// simple version gets wrong: OneMana's own deployment DOES run LiveKit, and if the
// backend happens to start before it is ready, a boot-time snapshot would report calls
// unavailable until somebody restarted the backend. Re-probing means a LiveKit that
// arrives late, or comes back after maintenance, heals itself within the TTL.
var reachable = helpers.NewTTLCache[bool](reachabilityTTL)

// Available reports whether calling can be offered right now.
//
// Cheap in the common case: an unconfigured host answers without touching the network,
// and a configured one answers from cache for the life of the TTL.
func Available() bool {
	if LiveKitService.Config == nil || strings.TrimSpace(LiveKitService.Config.HostURL) == "" {
		return false
	}
	if LiveKitService.LiveKitClient == nil {
		return false
	}
	if cached, ok := reachable.Get(cacheKey); ok {
		return cached
	}

	ok := probeReachable()
	reachable.Set(cacheKey, ok)
	return ok
}

// probeReachable asks LiveKit for its room list, the same call ConnectLiveKit uses.
//
// ListRooms is the right probe because it exercises what calling actually needs: the host
// resolves, the port answers, and the API key is accepted. A TCP dial would pass with
// wrong credentials and leave the client to discover that when a user tried to join.
func probeReachable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	_, err := LiveKitService.LiveKitClient.ListRooms(ctx, &livekit.ListRoomsRequest{})
	return err == nil
}

// noteReachability seeds the cache from a result already obtained, so a successful startup
// probe is not immediately repeated by the first config request.
func noteReachability(ok bool) {
	reachable.Set(cacheKey, ok)
}

// init announces calling to the feature registry that /config/client reports.
func init() {
	helpers.RegisterFeature(helpers.FeatureNameCalls, Available)
}
