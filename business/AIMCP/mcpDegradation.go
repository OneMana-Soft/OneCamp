package business

import (
	"sync"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// Connectors that are configured and enabled but contributing nothing.
//
// Each carries the topics a question would have to be about for its absence to
// have mattered, derived from the tool names it last introspected. A connector
// that is down is not a reason to warn somebody asking about lunch.
//
// The condition was already detected and already logged; what was missing was
// any path from the log to the person whose answer was thinner because of it.
// RebuildRegistry decides this on a background context, so the names are held
// here and read back on whichever request needs to explain itself.
var (
	unreachableMu      sync.RWMutex
	unreachableServers []ai.DegradedCapability
)

func init() {
	ai.RegisterDegradationReporter("mcp", func() []ai.DegradedCapability {
		unreachableMu.RLock()
		defer unreachableMu.RUnlock()
		// Copied out: the caller must not be able to mutate the live slice, and
		// a reader must not see a half-written one.
		out := make([]ai.DegradedCapability, len(unreachableServers))
		copy(out, unreachableServers)
		return out
	})
}

// setUnreachableServers replaces the published list. Called with the registry
// swap so the advertised tools and the explanation for their absence change
// together.
func setUnreachableServers(names []ai.DegradedCapability) {
	unreachableMu.Lock()
	unreachableServers = names
	unreachableMu.Unlock()
}
