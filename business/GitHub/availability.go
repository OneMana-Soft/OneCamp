package business

// Whether GitHub is connected, as a feature every member can read.
//
// WHY THIS EXISTS. The task panel shows a pull-request affordance only when the
// workspace has GitHub connected, and it learned that by calling
// /admin/github/status. That route lives in the admin router, so every member
// who was not an admin got a 403 on every task they opened. The panel then
// treated the failure as "not connected", which happened to look right and hid
// the fault, while the browser console filled with forbidden requests.
//
// "Is GitHub connected" is not privileged. It is a workspace fact a member needs
// in order to know whether an affordance applies to them, which is exactly what
// the feature registry is for. The admin endpoint keeps its full status, repos
// and rate limits included; only the boolean moves.
//
// TTL-CACHED, following initializers/livekitInit. FeatureStatus is evaluated on
// every client-config request, and this probe reads the integrations table. A
// short cache keeps that from becoming a query per page load while still letting
// a newly connected (or disconnected) GitHub show up on its own.

import (
	"context"
	"time"

	"github.com/google/uuid"

	integrationDomain "github.com/akashc777/OneCamp/domain/Integration"
	"github.com/akashc777/OneCamp/helpers"
)

const (
	// connectedTTL bounds how often the probe touches the database. Short enough
	// that connecting GitHub lights the feature up without a reload loop, long
	// enough that a busy workspace is not querying per request.
	connectedTTL = 30 * time.Second
	// probeTimeout keeps one probe from holding a config request open.
	probeTimeout = 2 * time.Second
	// connectedCacheKey — one subsystem, one entry.
	connectedCacheKey = "github-connected"
)

var connectedCache = helpers.NewTTLCache[bool](connectedTTL)

// Connected reports whether an admin has connected GitHub for this workspace.
//
// Errors are reported as NOT connected. A probe that cannot answer must hide the
// affordance rather than offer one that will fail, and this runs while a user is
// waiting for a page.
func Connected() bool {
	if cached, ok := connectedCache.Get(connectedCacheKey); ok {
		return cached
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	integration, err := integrationDomain.GetIntegration(ctx, "org", uuid.Nil, "github")
	ok := err == nil && integration != nil && integration.AccessToken != nil

	connectedCache.Set(connectedCacheKey, ok)
	return ok
}

// init announces the feature. Linking this package is what makes it visible, the
// same property helpers/features.go was built for.
func init() {
	helpers.RegisterFeature(helpers.FeatureNameGitHub, Connected)
}
