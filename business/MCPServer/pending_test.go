package business

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
)

// The status mapping is where the judgement lives, and two of its cases are the ones
// that would cause real damage if read the other way. Asserted by reading the source
// rather than by exercising the store, because the risky part is the mapping itself and
// it is a fixed table: a live database would prove the query works while saying nothing
// about whether "executing" was classified correctly.
func TestExecutingCountsAsAppliedAndFailedDoesNot(t *testing.T) {
	raw, err := os.ReadFile("pending.go")
	if err != nil {
		t.Fatalf("read pending.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

	// StatusExecuting must be grouped with StatusExecuted, not left to fall through.
	//
	// A call that is MID-EXECUTION must not be startable again by a retry arriving a
	// moment later. Reading "in progress" as "not yet done" is precisely how a
	// duplicate write happens, and it is the reading a careless mapping would take
	// because the work genuinely has not finished.
	appliedArm := regexp.MustCompile(`case\s+pendingModels\.StatusExecuting\s*,\s*pendingModels\.StatusExecuted\s*:`)
	if !appliedArm.MatchString(src) {
		t.Error("StatusExecuting is not grouped with StatusExecuted as ExistingApplied. A " +
			"retry arriving while the first call is still running would start the write a " +
			"second time.")
	}

	// StatusFailed must map to ExistingNone so the key is reusable.
	//
	// A write that errored did not take effect. Treating it as applied would leave an
	// agent permanently unable to retry after a transient fault, with the failure
	// looking like success.
	failedIdx := strings.Index(src, "pendingModels.StatusFailed")
	if failedIdx < 0 {
		t.Fatal("StatusFailed is not handled, so it falls to the default arm and is read as " +
			"awaiting approval — a failed write would wait forever for a person to approve " +
			"something that already ran and errored")
	}
	window := src[failedIdx:min(failedIdx+200, len(src))]
	if !strings.Contains(window, "ExistingNone") {
		t.Error("StatusFailed does not map to ExistingNone. A write that errored did not " +
			"take effect, so its key must be free for a retry.")
	}
}

// An unrecognised status must defer to a human, not proceed.
//
// New statuses get added to the pending-action store by someone thinking about that
// store, not about this mapping. The conservative reading of "I do not know what this
// means" is to ask a person — proceeding would execute, and reporting applied would
// claim something untrue.
func TestUnknownStatusDefersToAHuman(t *testing.T) {
	raw, err := os.ReadFile("pending.go")
	if err != nil {
		t.Fatalf("read pending.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

	def := strings.Index(src, "default:")
	if def < 0 {
		t.Fatal("the status switch has no default arm, so an unrecognised status leaves the " +
			"state as its zero value")
	}
	if !strings.Contains(src[def:min(def+200, len(src))], "ExistingAwaitingApproval") {
		t.Error("an unrecognised status does not map to ExistingAwaitingApproval. The safe " +
			"reading of an unknown state is to ask a person rather than to proceed or to " +
			"claim the write already happened.")
	}
}

// An empty key short-circuits without touching the store.
//
// A naturally idempotent write derives no key, and PlanWrite never calls this for one —
// but a lookup on an empty key would match whatever row happens to have an empty key,
// which is the kind of accident that turns "no deduplication needed" into "deduplicated
// against something unrelated".
func TestEmptyKeyIsNoneWithoutALookup(t *testing.T) {
	got, err := CheckExistingWrite(context.Background(), "   ")
	if err != nil {
		t.Fatalf("an empty key must not be an error: %v", err)
	}
	if got.State != ExistingNone {
		t.Fatalf("state = %q, want %q; an empty key means nothing to deduplicate against",
			got.State, ExistingNone)
	}
	if got.PendingActionID != "" {
		t.Error("an empty key must not return a pending action id")
	}
}

// The store's status constants must all be accounted for.
//
// Ratchets the mapping against the source of truth: if a status is added to the model
// and not classified here, it silently takes the default. The default is safe, but
// "safe" for an approval status means blocking a write forever, which is a bug worth
// finding at build time.
func TestEveryPendingStatusIsClassified(t *testing.T) {
	raw, err := os.ReadFile("pending.go")
	if err != nil {
		t.Fatalf("read pending.go: %v", err)
	}
	src := string(raw)

	for _, status := range []string{
		pendingModels.StatusPending,
		pendingModels.StatusExecuting,
		pendingModels.StatusExecuted,
		pendingModels.StatusRejected,
		pendingModels.StatusExpired,
		pendingModels.StatusFailed,
	} {
		// The constants are named Status<Titlecase>, which is how they appear here.
		name := "Status" + strings.ToUpper(status[:1]) + status[1:]
		if !strings.Contains(src, "pendingModels."+name) {
			t.Errorf("pending status %q (pendingModels.%s) is not classified in "+
				"CheckExistingWrite, so it falls to the default and is read as awaiting "+
				"approval — which for a terminal status means the write is blocked forever",
				status, name)
		}
	}
}
