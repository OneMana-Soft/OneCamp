package helpers

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The property this exists for: an undo must still run when the context that reached it is
// already cancelled.
//
// This is not a hypothetical. Compensation happens because a later step failed, and by then the
// request context is frequently cancelled or on its way out. Passing that context to the undo
// means the reversal fails exactly when it is needed, leaving the half-written state this
// function was written to clean up.
func TestCompensateRunsEvenWhenTheCallerContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller's context is already dead

	ran := false
	var seenErr error
	err := CompensateOnFailure(ctx, "create channel row", func(undoCtx context.Context) error {
		ran = true
		seenErr = undoCtx.Err()
		return nil
	})

	if !ran {
		t.Fatal("undo did not run on a cancelled caller context — the compensation would never " +
			"happen in the situation it exists for")
	}
	if seenErr != nil {
		t.Errorf("undo saw a cancelled context (%v); it must be detached from the caller's", seenErr)
	}
	if err != nil {
		t.Errorf("CompensateOnFailure returned %v, want nil when the undo succeeded", err)
	}
}

// The undo context must still carry a deadline. Detached does not mean unbounded: an undo runs
// on an already-failing request, and one that hangs turns a failed write into a stuck handler.
func TestCompensateGivesTheUndoADeadline(t *testing.T) {
	var deadline time.Time
	var ok bool

	_ = CompensateOnFailure(context.Background(), "x", func(undoCtx context.Context) error {
		deadline, ok = undoCtx.Deadline()
		return nil
	})

	if !ok {
		t.Fatal("undo context has no deadline; a hanging undo would hang the caller")
	}
	if until := time.Until(deadline); until <= 0 || until > compensateTimeout+time.Second {
		t.Errorf("undo deadline is %v away, want between 0 and %v", until, compensateTimeout)
	}
}

// A values carried on the caller's context must survive, because logging and tracing depend on
// them and a compensation is exactly the event you want correlated.
func TestCompensatePreservesContextValues(t *testing.T) {
	type ctxKey string
	const key ctxKey = "request-id"

	ctx := context.WithValue(context.Background(), key, "abc-123")
	ctx, cancel := context.WithCancel(ctx)
	cancel()

	var seen any
	_ = CompensateOnFailure(ctx, "x", func(undoCtx context.Context) error {
		seen = undoCtx.Value(key)
		return nil
	})

	if seen != "abc-123" {
		t.Errorf("context value lost through compensation: got %v, want abc-123", seen)
	}
}

// A failed undo must be reported, because that is the branch that genuinely leaves state behind.
// The returned error wraps the undo's own error so a caller can inspect it.
func TestCompensateReportsAFailedUndo(t *testing.T) {
	sentinel := errors.New("delete refused by foreign key")

	err := CompensateOnFailure(context.Background(), "create channel row",
		func(context.Context) error { return sentinel })

	if err == nil {
		t.Fatal("a failed undo returned nil — the orphan would be invisible")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error does not wrap the undo's error: %v", err)
	}
	// The operation name belongs in the message; a bare "delete failed" is unactionable.
	if got := err.Error(); got == sentinel.Error() {
		t.Errorf("error lost the operation name: %q", got)
	}
}

// A nil undo is a programming mistake at the call site, not a data problem, and must not be
// silently treated as a successful reversal.
func TestCompensateRejectsANilUndo(t *testing.T) {
	if err := CompensateOnFailure(context.Background(), "x", nil); err == nil {
		t.Error("a nil undo reported success; the caller would believe state had been reversed")
	}
}

// A nil context must not panic. Compensation runs on error paths, which are the least-exercised
// paths in any codebase.
func TestCompensateToleratesANilContext(t *testing.T) {
	ran := false
	//nolint:staticcheck // deliberately passing a nil context: this asserts the guard.
	err := CompensateOnFailure(nil, "x", func(context.Context) error { ran = true; return nil })
	if !ran || err != nil {
		t.Errorf("nil context: ran=%v err=%v, want ran=true err=nil", ran, err)
	}
}

// A Dgraph write is a failure if EITHER signal is bad, and the uid half is the one that gets
// forgotten: a mutation can return no error and still not have created the node.
//
// This is pinned because the condition used to be written by hand at each call site and one of
// them had `&&` where it needed `||`, which caught only the case where both signals were bad. A
// Dgraph error that still returned a uid, and an empty uid with no error, both slipped through —
// and the caller went on to write notification rows and a search document for a project that did
// not exist, then reported success.
func TestDgraphWriteFailed(t *testing.T) {
	boom := errors.New("dgraph unavailable")

	cases := []struct {
		name string
		uid  string
		err  error
		want bool
	}{
		{"a uid and no error is the only success", "0x1a2b", nil, false},
		{"an error with no uid fails", "", boom, true},
		// The two the broken && missed.
		{"an EMPTY UID WITH NO ERROR still fails", "", nil, true},
		{"an error that still returned a uid still fails", "0x1a2b", boom, true},
		// Whitespace is not a uid. Dgraph does not return one, but a caller trimming or
		// concatenating could produce it, and " " is not a node.
		{"a blank uid fails", "   ", nil, true},
	}

	for _, c := range cases {
		if got := DgraphWriteFailed(c.uid, c.err); got != c.want {
			t.Errorf("%s: DgraphWriteFailed(%q, %v) = %v, want %v", c.name, c.uid, c.err, got, c.want)
		}
	}
}
