package helpers

import (
	"context"
	"sort"
	"sync"
)

// Demo fixtures a subsystem contributes to the curated demo workspace.
//
// WHY INVERTED. The curated demo lives in business/DemoSeed, which is on every
// edition. The governance drill's fixture lives in business/AIDrill, which is on
// the AI edition only. A direct call would either drag the AI package onto the
// AI-free line or need a build tag, and both are how an edition split starts
// leaking.
//
// So a subsystem announces its own fixture from init(), exactly as system checks,
// retention sweepers and evidence sections already do. Linking the package is
// what makes the fixture exist; on the AI-free build nothing registers and the
// seeder runs the rest without knowing anything was missing.
//
// It exists because the demo shipped a drill nobody could see: the code was live
// and reachable and the workspace had no fixture, so the one button the whole
// pitch rests on said "Set it up" to every visitor.
type DemoFixture struct {
	// Name is what the seeder prints, so an operator can tell which subsystem
	// contributed what.
	Name string
	// Describe says what the fixture is for, in one line.
	Describe string
	// Seed creates it. Must be idempotent: the seeder is expected to be re-run,
	// and a fixture that doubles on the second pass is worse than none.
	Seed func(ctx context.Context, adminEmail string) error
}

var (
	demoFixtureMu sync.RWMutex
	demoFixtures  = map[string]DemoFixture{}
)

// RegisterDemoFixture records a fixture the demo seeder should create. Call it
// from package init. Re-registering the same name replaces it, which keeps tests
// hermetic and means a package that registers twice still seeds once.
func RegisterDemoFixture(f DemoFixture) {
	if f.Name == "" || f.Seed == nil {
		return
	}
	demoFixtureMu.Lock()
	defer demoFixtureMu.Unlock()
	demoFixtures[f.Name] = f
}

// DemoFixtures returns the registered fixtures in a stable order, so two runs
// seed in the same sequence and the output reads the same way.
func DemoFixtures() []DemoFixture {
	demoFixtureMu.RLock()
	defer demoFixtureMu.RUnlock()
	out := make([]DemoFixture, 0, len(demoFixtures))
	for _, f := range demoFixtures {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
