package helpers

import (
	"context"
	"sort"
	"sync"
	"time"
)

// A registry of stores that hold detailed records and can shorten how long they
// keep them.
//
// WHY AN INVERSION. Retention is one policy over several stores, and the sweep
// that applies it lives in the audit package, which BOTH editions ship. The
// agent run ledger is one of those stores and belongs to the AI packages, which
// only one edition ships. Calling it directly made a shared package import AI
// code, which on the AI-free edition is not a style argument but a build break:
// those packages are not there.
//
// So the sweep no longer knows what it is sweeping. A store announces itself
// from its own package's init, which means linking that package is what puts
// its records under retention, and not linking it is what makes them absent.
// Exactly the property the feature registry next door relies on.
//
// The audit log registers itself the same way rather than being a hardcoded
// special case, so there is one code path and every store is described the same
// way in the log.
type RetentionSweeper struct {
	// Name reaches an operator's log line, so it reads as the thing they would
	// recognise ("agent run") rather than as a package path.
	Name string
	// Note says what survives the redaction. Each store keeps something
	// different and the difference is the whole reassurance: someone reading
	// "the content of this entry is gone" needs to know in the same breath that
	// the hash chain still verifies, or they will suspect tampering.
	Note string
	// Sweep redacts records older than cutoff and reports how many it touched.
	Sweep func(context.Context, time.Time) (int64, error)
}

var (
	retentionMu       sync.RWMutex
	retentionSweepers = map[string]RetentionSweeper{}
)

// RegisterRetentionSweeper records a store that honours the retention window.
// Call it from package init.
//
// Re-registering the same name replaces it, which keeps tests hermetic and
// means a package that somehow registers twice still sweeps once.
func RegisterRetentionSweeper(name, note string, sweep func(context.Context, time.Time) (int64, error)) {
	if name == "" || sweep == nil {
		return
	}
	retentionMu.Lock()
	defer retentionMu.Unlock()
	retentionSweepers[name] = RetentionSweeper{Name: name, Note: note, Sweep: sweep}
}

// RetentionSweepers returns the registered stores, in a stable order so the log
// reads the same way on every pass.
func RetentionSweepers() []RetentionSweeper {
	retentionMu.RLock()
	out := make([]RetentionSweeper, 0, len(retentionSweepers))
	for _, s := range retentionSweepers {
		out = append(out, s)
	}
	retentionMu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
