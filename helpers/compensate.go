package helpers

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// compensateTimeout bounds an undo. It is deliberately short: an undo runs on a request that is
// already failing, and a compensation that hangs turns one failed write into a stuck handler.
const compensateTimeout = 10 * time.Second

// CompensateOnFailure reverses a step that already succeeded, because a later step in the same
// logical operation failed.
//
// WHY THIS EXISTS, rather than an inline delete at each site. OneCamp writes some entities to
// Postgres and Dgraph in sequence, and there is no transaction spanning both. When the second
// write fails, the first one has already happened, and "log it and return" leaves a row that no
// part of the product can see. For a table with a UNIQUE column that is worse than untidy: the
// invisible row keeps the name, so the user cannot retry with it — one transient failure makes
// the operation permanently unrepeatable.
//
// Two details matter, and both are easy to get wrong at a call site:
//
// A DETACHED CONTEXT. undo runs on context.WithoutCancel(ctx) with its own deadline. The ctx
// that reached this point is usually the one that just failed or is being cancelled, and
// handing it to the undo means the compensation fails precisely when it is needed. This is the
// single most important line in the file.
//
// THE CALLER'S ERROR IS NOT TOUCHED. This returns the UNDO's error, not the original failure.
// Callers keep and return their own error; the undo error only says whether the reversal
// worked. A caller that ignores the return still gets the log, because a failed undo is the
// case that actually leaves an orphan behind and it must be findable afterwards.
//
// what names the operation being reversed, and appears in the log.
func CompensateOnFailure(ctx context.Context, what string, undo func(context.Context) error) error {
	if undo == nil {
		return fmt.Errorf("compensate %s: no undo provided", what)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	undoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensateTimeout)
	defer cancel()

	if err := undo(undoCtx); err != nil {
		// Loudly, and with the word "orphan", because this is the branch that leaves state
		// behind and somebody will be searching the logs for it later.
		LogErrorWithContext(undoCtx,
			"helpers/CompensateOnFailure could not reverse %s — ORPHANED STATE LEFT BEHIND, "+
				"manual cleanup may be required: %+v", what, err)
		return fmt.Errorf("compensate %s: %w", what, err)
	}

	// Recorded at info, not debug: a compensation means a write failed halfway, which is worth
	// seeing in normal logs even when the reversal worked.
	LogInfoWithContext(undoCtx, "helpers/CompensateOnFailure reversed %s after a later step failed", what)
	return nil
}

// DgraphWriteFailed reports whether a Dgraph create-or-update did not take effect.
//
// The non-obvious half is the uid: a mutation can come back with NO ERROR and still not have
// created the node, in which case the returned uid is empty. Treating only err as the signal
// reports success for a node that does not exist — the caller then carries on writing
// notifications, search documents and edges that point at nothing, and tells the user it worked.
//
// This exists as one named rule because it was previously written out by hand at each call site
// and one of them got the boolean backwards: `len(uid) == 0 && err != nil` caught only the case
// where BOTH were bad, so a Dgraph error that still returned a uid, and an empty uid with no
// error, both walked past the check. A single function cannot disagree with itself.
func DgraphWriteFailed(uid string, err error) bool {
	return err != nil || strings.TrimSpace(uid) == ""
}
