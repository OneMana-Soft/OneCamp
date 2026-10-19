//go:build integration

// Package processcache is where a package that keeps a process-wide cache of
// what its databases hold says how to forget it. The integration harness
// forgets every registered cache as each top-level test starts
// (tests/integration/teststate.go). A new process starts with them empty, and
// every test there has databases of its own.
//
// A package registers from an init() in a file of its own behind the
// integration tag, so the registration never reaches the shipped binary:
//
//	//go:build integration
//
//	func init() { processcache.Register("business/User: the automation bot", ForgetAutomationBotForTest) }
//
// Why a registry rather than a list in the harness: the harness used to call
// each package's Forget…ForTest itself. So it imported those four packages,
// and through them about 130 more. A package among them with an integration
// test inside the package (package x, not x_test) imports the harness, and
// Go refuses that import cycle. Thirteen tests in eight packages stopped
// compiling, and the only build that could show it (the integration tag) was
// one no gate ran. Now the dependency runs the other way. The harness imports
// this package and no business or domain package, and a cache is forgotten in
// every test binary that links the package keeping it. A binary that doesn't
// link the package doesn't have the cache. helpers/integrationHarnessGuard_test.go
// fails when the harness imports a business or domain package again, or when
// a package's Forget…ForTest isn't registered here.
//
// This package imports nothing of the module's, so anything may import it.
package processcache

import (
	"fmt"
	"sync"
)

type entry struct {
	name   string
	forget func()
}

var registered struct {
	mu      sync.Mutex
	entries []entry
}

// Register adds a cache to forget before each integration test. name says
// whose cache it is ("business/Settings: the workspace settings"). An empty
// name, a nil func or a name already taken panics. Registration happens in
// init, so a mistake fails the test binary at its start instead of leaving a
// cache that is never forgotten.
func Register(name string, forget func()) {
	if name == "" || forget == nil {
		panic(fmt.Sprintf("processcache: %q needs a name and a way to forget", name))
	}
	registered.mu.Lock()
	defer registered.mu.Unlock()
	for _, e := range registered.entries {
		if e.name == name {
			panic(fmt.Sprintf("processcache: %q is registered twice", name))
		}
	}
	registered.entries = append(registered.entries, entry{name: name, forget: forget})
}

// ForgetAll forgets every registered cache, in the order they registered.
func ForgetAll() {
	registered.mu.Lock()
	entries := append([]entry(nil), registered.entries...)
	registered.mu.Unlock()
	for _, e := range entries {
		e.forget()
	}
}
