package helpers

// A replica's cached view of admin-edited configuration, kept honest by the
// store rather than by the process that took the edit.
//
// WHY THIS EXISTS. Three things are built once per process and rebuilt when an
// admin saves them: the AI service (provider, model, key), the MCP tool
// registry, and the compiled workflow rules. Each rebuild is triggered from the
// HTTP handler that took the save, so it runs in exactly one process. With one
// replica that is every process. With two, the second keeps yesterday's
// provider, yesterday's tools and yesterday's rules until somebody restarts
// it, and nothing says so: the agents on that replica simply behave as they
// did before the save. The queues were made safe for several replicas some
// time ago; these caches were not, and a queue that is shared across replicas
// running agents against a config that is not is worse than either alone.
//
// The handler-side rebuild stays, so the replica that took the save applies it
// at once. This loop is what makes every OTHER replica converge: it asks the
// store for a cheap fingerprint of the rows the cache was built from and
// rebuilds only when that changes. The fingerprint is the store's account, not
// a message from the process that saved, so it also covers a save made while
// this process was down, and it needs no bus, no broker and no second code path
// for "tell the others".
//
// This is the shape Google's AX controllers take, for the same reason: a
// reconciler reads desired state from the store on every pass and never trusts
// that it was told about a change.

import (
	"context"
	"fmt"
	"time"
)

// DefaultConfigReconcileInterval is how long another replica can run on a
// superseded config. Thirty seconds is well under the time an admin takes to
// save a setting and then try it, and the query it drives is a count and a max
// over a handful of rows.
const DefaultConfigReconcileInterval = 30 * time.Second

// ConfigReconciler describes one cache and how to check it against the store.
type ConfigReconciler struct {
	// Name appears in logs.
	Name string
	// Interval is how often the store is asked. Every replica asks, so this is
	// a per-replica cost; the query it drives should be a count and a max.
	Interval time.Duration
	// Fingerprint returns a value that changes whenever the rows the cache is
	// built from change, and only then.
	Fingerprint func(ctx context.Context) (string, error)
	// Apply rebuilds the cache from the store. It is the same function the
	// admin handler calls, so a rebuild here is indistinguishable from a save.
	Apply func(ctx context.Context) error
}

// reconcileState is what one loop remembers between passes.
type reconcileState struct {
	fingerprint string
	// baselined is false until the store has been read once. The first
	// reading is recorded and never applied: the caller starts the loop right
	// after it built the cache from the store, so at that moment the two agree,
	// and a rebuild would only discard the state a rebuild carries with it (the
	// AI service's circuit breaker and rate limiter among others). Applying on
	// the first pass regardless was the simpler design, and wrong for exactly
	// that reason.
	baselined bool
}

// reconcileStep is one observation. Pure, so the decision can be tested
// without a clock or a store: given what the store says now and what it said
// last time, it returns what to remember next and whether the cache was
// rebuilt.
//
// On an Apply failure the OLD fingerprint is kept, so the next pass sees the
// change again and retries. Recording the new one would make a rebuild that
// failed look like one that succeeded, permanently.
func reconcileStep(ctx context.Context, r ConfigReconciler, st reconcileState) (reconcileState, bool, error) {
	fp, err := r.Fingerprint(ctx)
	if err != nil {
		return st, false, fmt.Errorf("%s: fingerprint: %w", r.Name, err)
	}
	if !st.baselined {
		return reconcileState{fingerprint: fp, baselined: true}, false, nil
	}
	if fp == st.fingerprint {
		return st, false, nil
	}
	if err := r.Apply(ctx); err != nil {
		return st, false, fmt.Errorf("%s: apply: %w", r.Name, err)
	}
	return reconcileState{fingerprint: fp, baselined: true}, true, nil
}

// validate rejects a reconciler that could never do its job, so the mistake
// is a boot-time error rather than a loop that runs forever doing nothing.
func (r ConfigReconciler) validate() error {
	switch {
	case r.Name == "":
		return fmt.Errorf("config reconciler needs a name")
	case r.Interval <= 0:
		return fmt.Errorf("config reconciler %s: interval must be positive", r.Name)
	case r.Fingerprint == nil:
		return fmt.Errorf("config reconciler %s: no fingerprint function", r.Name)
	case r.Apply == nil:
		return fmt.Errorf("config reconciler %s: no apply function", r.Name)
	}
	return nil
}

// StartConfigReconciler baselines the cache against the store now and then
// keeps it converged until ctx is done.
//
// CALL IT RIGHT AFTER BUILDING THE CACHE. The baseline is taken synchronously,
// before this returns, so the ordering "build, then start" is what guarantees
// the first reading describes the rows the cache was built from. A change that
// lands between the two is missed until the next change; the window is the
// time between two adjacent statements at boot.
//
// A baseline that fails (the database was not answering at that instant) is
// logged and the loop starts unbaselined, so the first successful reading
// becomes the baseline instead.
func StartConfigReconciler(ctx context.Context, r ConfigReconciler) error {
	if err := r.validate(); err != nil {
		return err
	}
	st, _, err := reconcileStep(ctx, r, reconcileState{})
	if err != nil {
		LogErrorWithContext(ctx, "config reconciler %s: baseline: %v (will baseline on first successful read)", r.Name, err)
	}
	go func() {
		ticker := time.NewTicker(r.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			next, applied, err := reconcileStep(ctx, r, st)
			if err != nil {
				if ctx.Err() == nil {
					LogErrorWithContext(ctx, "config reconciler: %v", err)
				}
				continue
			}
			if applied {
				LogInfoWithContext(ctx, "config reconciler %s: rows changed in the store; cache rebuilt", r.Name)
			}
			st = next
		}
	}()
	return nil
}
