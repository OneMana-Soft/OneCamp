package integration

// The binary's end. Unlike the rest of the harness, Main and the stop list
// carry no build tag: a package whose one TestMain also serves its unit tests
// (business/AIAgent's sets loggers up for both) calls Main in either build.
// Nothing here reaches Docker, and a unit build starts no server, so there
// Main is m.Run().

import (
	"sync"
	"testing"
)

// Main runs the binary's tests, then stops every server they started,
// whatever the tests' result, and returns the binary's exit code. Every
// package whose tests run on the harness calls it from its TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(integration.Main(m)) }
//
// helpers/integrationHarnessGuard_test.go fails a package that doesn't.
//
// WHY. testcontainers' reaper removes a run's containers when the run ends,
// and the run is the whole `go test`, not one package's binary. Until Main,
// only the harness's own binary stopped its servers: on 24 Oct 2026 one run
// over many packages kept every finished binary's Dgraph, Postgres,
// OpenSearch and Redis up, about 30 Dgraphs at once, and the workstation ran
// out of memory. The reaper stays the backstop for a binary that is killed or
// panics.
func Main(m *testing.M) int {
	defer stopServers()
	return m.Run()
}

// stopList is the started servers' stops, in the order the servers started.
// Only the integration build starts servers, so add is there (shared.go).
type stopList struct {
	mu  sync.Mutex
	fns []func()
}

var stops stopList

// stopServers stops every server the binary started, the last started first.
func stopServers() {
	stops.mu.Lock()
	fns := stops.fns
	stops.fns = nil
	stops.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}
