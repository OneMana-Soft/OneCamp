package business

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
)

// goSafe runs fn in a goroutine with panic recovery so an OpenSearch
// indexing failure (or any other async side-effect) doesn't take the
// whole process down. The recovered panic is logged with context so
// it's visible in the structured log stream but doesn't escape.
//
// Use this for fire-and-forget side-effects that the import pipeline
// doesn't strictly need to commit:
//   - OpenSearch indexing (best-effort; reconcileable)
//   - MQTT publishes (transient broker failures recover on next event)
//   - Cache invalidation
//
// DO NOT use this for writes the import depends on for correctness
// (Postgres / Dgraph). Those go on the main worker goroutine and
// surface their errors back through the chunk's error column.
func goSafe(ctx context.Context, name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				helpers.LogErrorWithContext(ctx,
					"Import async %s recovered panic: %v", name, r)
			}
		}()
		fn()
	}()
}
