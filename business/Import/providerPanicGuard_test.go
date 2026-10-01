package business

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every channel-producing goroutine in an import provider must carry a panic
// guard, and it must be registered in the one order that works.
//
// WHY THIS IS A TEST AND NOT A CONVENTION
// ---------------------------------------
// The providers stream from third-party APIs and decode responses whose shape
// they do not control, which is where an index-out-of-range or a failed type
// assertion actually comes from. An unrecovered panic in ANY goroutine
// terminates the whole Go process, and the orchestrator's recover cannot help
// because it sits on a different goroutine. So one malformed upstream response
// takes the workspace down for every user — a failure with no stack in the
// request log and no obvious link to the import that caused it.
//
// The ordering half matters just as much as the presence half. Defers run LIFO,
// so the guard has to be registered AFTER the channel closes in order to run
// BEFORE them, while errCh is still open:
//
//	defer close(out)
//	defer close(errCh)
//	defer helpers.RecoverToErr("jira.IterTasks", errCh) // last = runs first
//
// Registered above the closes it would run last, after errCh was closed, and its
// send would panic on a closed channel — the guard becoming a second crash. That
// is a silent, plausible mistake for someone adding a provider, which is exactly
// what a test should catch.
var (
	closeErrCh = regexp.MustCompile(`^\s*defer close\(errCh\)\s*$`)
	closeOut   = regexp.MustCompile(`^\s*defer close\(\w+\)\s*$`)
	guardLine  = regexp.MustCompile(`^\s*defer helpers\.RecoverToErr\("([^"]*)",\s*errCh\)\s*$`)
)

func providerSources(t *testing.T) map[string][]string {
	t.Helper()
	root := filepath.Join("providers")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("cannot read provider dir: %v", err)
	}
	sources := map[string][]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), "provider.go")
		b, err := os.ReadFile(path)
		if err != nil {
			continue // not every dir is a provider with this layout
		}
		sources[path] = strings.Split(string(b), "\n")
	}
	if len(sources) == 0 {
		t.Fatal("found no provider sources; this test would pass vacuously")
	}
	return sources
}

func TestProviderProducersGuardAgainstPanics(t *testing.T) {
	for path, lines := range providerSources(t) {
		for i, line := range lines {
			if !closeErrCh.MatchString(line) {
				continue
			}
			// The guard must be the very next line.
			if i+1 >= len(lines) || !guardLine.MatchString(lines[i+1]) {
				t.Errorf(
					"%s:%d: `defer close(errCh)` is not followed by "+
						"`defer helpers.RecoverToErr(...)`. Without it a panic while "+
						"decoding a third-party response kills the whole server, and "+
						"the orchestrator's recover cannot reach this goroutine.",
					path, i+1)
			}
		}
	}
}

func TestProviderPanicGuardsAreRegisteredAfterTheCloses(t *testing.T) {
	for path, lines := range providerSources(t) {
		for i, line := range lines {
			m := guardLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			// Registered last means the line ABOVE is a close.
			if i == 0 || !closeOut.MatchString(lines[i-1]) {
				t.Errorf(
					"%s:%d: RecoverToErr must be deferred AFTER the channel closes "+
						"so LIFO runs it first, while errCh is still open. Registered "+
						"before them it runs last and its send panics on a closed "+
						"channel, turning the guard into a second crash.",
					path, i+1)
			}
			// A label is what makes a recovered panic actionable in a log.
			if strings.TrimSpace(m[1]) == "" {
				t.Errorf("%s:%d: RecoverToErr needs a name so the log identifies the producer", path, i+1)
			}
		}
	}
}
