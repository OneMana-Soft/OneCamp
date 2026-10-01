package business

// Exactly-once for writes that nobody approves.
//
// THE HOLE THIS FILLS. write.go derives an idempotency key for every mutating call
// that is not naturally idempotent, and PlanWrite checks whether a prior call with that
// key exists. For a DESTRUCTIVE write that works, because asking for approval persists
// a row keyed on it. For an ADDITIVE write — send a message, add a table row — PlanWrite
// returned "proceed" and nothing ever wrote the key down. So the retry a client is
// permitted to make after losing a response derived the same key, found no row, and did
// the work again.
//
// The result was the exact duplicate the key exists to prevent, on precisely the tools
// where a duplicate is most visible to a customer: two identical messages in a channel,
// two identical rows in a table.
//
// CLAIM BEFORE, SETTLE AFTER. The key is reserved before the handler runs, not after.
// Recording it afterwards would leave two concurrent retries both executing, because
// nothing would separate them until both were done. Reserving first makes the database's
// unique index the arbiter: the second arrival loses the insert and is answered from the
// first one's state.
//
// A FAILED WRITE FREES THE KEY. Settling a failure as 'failed' is read back as "no prior
// call", so an agent can retry after a transient fault. Refusing forever would make one
// network blip permanently unrepeatable, which is a worse failure than the duplicate
// being guarded against.
//
// NO APPROVAL CARD IS CREATED. The claim lands directly in 'executing', which the
// open-actions query does not select, so reserving a key never shows a human something
// to approve. The approval path is still the approval path; this is only bookkeeping,
// and conflating the two would put phantom cards in front of people.
//
// REUSES ai_pending_actions rather than adding a table, for the same reason the approval
// path does: the unique partial index on idempotency_key already is the deduplication
// primitive, and a second store would be a second answer to "did this already happen".

import (
	"context"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	"github.com/google/uuid"
)

// claimTTL bounds how long a claim row stays relevant: the window in which a retry of
// the SAME call is recognised as a duplicate rather than performed again. Not a deadline
// for the execution, which the request itself bounds.
//
// SHORT ON PURPOSE, and the direction matters more than the exact number. The two errors
// are not equally bad:
//
//   - Window too SHORT: a very late retry runs again, producing a duplicate. Visible in
//     the channel or the table, and a person can delete it.
//   - Window too LONG: a genuine second identical action is answered "already applied"
//     and never happens. The caller is told it succeeded. Silent, and nobody knows to
//     look.
//
// So the window should cover realistic RETRY behaviour and nothing more. A client retries
// a lost response in seconds; a queued job with backoff might take minutes. Fifteen
// minutes covers both generously.
//
// This started at six hours, which was wrong for the creations. Task names and message
// text repeat naturally — "follow up", "on it" — so a long window makes the silent
// failure the likely one: a second genuine "create task: follow up" the same afternoon
// would have been swallowed. A retry window has no reason to be measured in hours.
const claimTTL = 15 * time.Minute

// ClaimResult is the outcome of reserving a key.
type ClaimResult struct {
	// Outcome is WriteProceed when the caller now owns the key and must run the
	// handler, or the existing call's outcome when it does not.
	Outcome WriteOutcome
	// Reason is always populated so the audit row and the client message agree.
	Reason string
	// ClaimID identifies the reserved row. Non-empty only on WriteProceed, and must be
	// handed to SettleWrite once the handler returns.
	ClaimID string
}

// claimAttempt is one reservation's parameters, bundled so the store seam below takes a
// value rather than eight positional arguments that are easy to transpose.
type claimAttempt struct {
	Principal   uuid.UUID
	SurfaceType string
	SurfaceID   string
	Tool        string
	Params      map[string]string
	Description string
	Key         string
	ExpiresAt   time.Time
}

// claimStore is the persistence ClaimWrite needs, injected rather than called directly.
//
// SAME REASON PlanWrite TAKES checkExisting AS A PARAMETER: the ORDER these are called in,
// and what is done with each answer, is the part that decides whether a write happens twice
// — and it is the part a database cannot help verify cheaply. Every branch here has a
// success, a not-applicable and an error path; with the calls injected, all of them are
// reachable in a unit test with no database and no container.
//
// The seam returns a claim ID and a boolean rather than a model row, so a fake needs to know
// nothing about the storage layer. ok=false means "the key was already taken" for Claim, and
// "the row was not in a retakeable state" for Retake — in both cases a real outcome, not an
// error.
type claimStore struct {
	Claim    func(ctx context.Context, a claimAttempt) (claimID string, ok bool, err error)
	Retake   func(ctx context.Context, a claimAttempt) (claimID string, ok bool, err error)
	Existing func(ctx context.Context, key string) (ExistingWrite, error)
}

// liveClaimStore is the real persistence, adapting the model's row-returning functions to
// the narrow seam above.
func liveClaimStore() claimStore {
	adapt := func(
		fn func(context.Context, uuid.UUID, string, string, string, map[string]string, string, string, time.Time) (*pendingModels.PendingAction, error),
	) func(context.Context, claimAttempt) (string, bool, error) {
		return func(ctx context.Context, a claimAttempt) (string, bool, error) {
			row, err := fn(ctx, a.Principal, a.SurfaceType, a.SurfaceID, a.Tool, a.Params,
				a.Description, a.Key, a.ExpiresAt)
			if err != nil {
				return "", false, err
			}
			if row == nil {
				return "", false, nil
			}
			return row.Id.String(), true, nil
		}
	}
	return claimStore{
		Claim:    adapt(pendingModels.ClaimIdempotencyKey),
		Retake:   adapt(pendingModels.RetakeIdempotencyKey),
		Existing: CheckExistingWrite,
	}
}

// ClaimWrite reserves the idempotency key for an additive write about to execute.
//
// Returns WriteProceed only when this call won the reservation. Every other outcome
// means an earlier arrival of the identical call already decided the matter, and the
// caller must NOT run the handler.
//
// Safe to call with an empty key: that means the tool is read-only or naturally
// idempotent, so there is nothing to deduplicate and the answer is simply proceed.
// Callers therefore need no branch of their own.
func ClaimWrite(ctx context.Context, spec *ToolSpec, call ToolCallContext, clientName, idempotencyKey string) ClaimResult {
	return claimWriteWith(ctx, spec, call, clientName, idempotencyKey, liveClaimStore())
}

// claimWriteWith is ClaimWrite's whole decision, against an injected store.
func claimWriteWith(ctx context.Context, spec *ToolSpec, call ToolCallContext, clientName, idempotencyKey string, store claimStore) ClaimResult {
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		return ClaimResult{Outcome: WriteProceed, Reason: "no deduplication needed"}
	}
	if spec == nil {
		return ClaimResult{Outcome: WriteRefused, Reason: "no tool spec"}
	}

	principal, perr := uuid.Parse(strings.TrimSpace(call.PrincipalUserID))
	if perr != nil {
		// The claim row is attributed to the accountable human, so without one there is
		// nothing to attribute it to. Refusing rather than claiming anonymously.
		return ClaimResult{Outcome: WriteRefused, Reason: "this call has no accountable person to record the write against"}
	}

	// A DECLARED SURFACE, not the resource kind. This used to write
	// string(call.Resource.Kind) into a column whose vocabulary is channel/dm/group/
	// assistant — see surface.go.
	surfaceType, surfaceID := SurfaceForResource(call.Resource, call.PrincipalUserID)
	attempt := claimAttempt{
		Principal:   principal,
		SurfaceType: surfaceType,
		SurfaceID:   surfaceID,
		Tool:        spec.Name,
		Params:      StringifyArgs(call.Args),
		Description: ApprovalDescription(spec, call, clientName),
		Key:         key,
		ExpiresAt:   time.Now().Add(claimTTL),
	}

	claimID, claimed, err := store.Claim(ctx, attempt)
	if err != nil {
		// Cannot establish whether this is a duplicate. Refuse: performing the write
		// twice is worse than not performing it, and the caller may retry when the
		// store is healthy. Same direction PlanWrite takes on a lookup failure.
		helpers.LogErrorWithContext(ctx, "business/MCPServer could not claim a write key for %s: %+v", spec.Name, err)
		return ClaimResult{
			Outcome: WriteRefused,
			Reason:  "could not reserve this call against duplicates; refusing rather than risking a duplicate write",
		}
	}

	if claimed {
		return ClaimResult{Outcome: WriteProceed, Reason: "reserved", ClaimID: claimID}
	}

	// THE KEY IS TAKEN — but that does not always mean the work happened.
	//
	// A settled row KEEPS its key: the unique index is on the key alone, not the key plus
	// a status. So a row left 'failed' by a transient error, or reclaimed as 'failed'
	// after a crash, blocks the insert forever while truthfully reporting that nothing
	// took effect. Without this step a single failure made that exact call permanently
	// unrepeatable, and the "a failed write frees the key" property this file claims would
	// have been false.
	//
	// Retaking is guarded to the states where nothing happened — failed and expired — and
	// deliberately refuses to retake a REJECTED row, because a human said no and a retry
	// must not quietly turn that into another attempt.
	retakenID, retaken, rerr := store.Retake(ctx, attempt)
	if rerr != nil {
		helpers.LogErrorWithContext(ctx, "business/MCPServer could not retake a write key for %s: %+v", spec.Name, rerr)
		return ClaimResult{
			Outcome: WriteRefused,
			Reason:  "could not reserve this call against duplicates; refusing rather than risking a duplicate write",
		}
	}
	if retaken {
		return ClaimResult{Outcome: WriteProceed, Reason: "retried after a previous attempt did not complete", ClaimID: retakenID}
	}

	// Not retakeable: applied, in flight, rejected, or awaiting a person. Read what the
	// earlier call became and answer from that, reusing the one mapping of store states to
	// write outcomes rather than interpreting statuses a second time here.
	existing, cerr := store.Existing(ctx, key)
	if cerr != nil {
		return ClaimResult{
			Outcome: WriteRefused,
			Reason:  "an identical call exists but its outcome could not be read; refusing rather than risking a duplicate write",
		}
	}
	switch existing.State {
	case ExistingApplied:
		return ClaimResult{Outcome: WriteAlreadyApplied, Reason: "an identical call was already applied", ClaimID: existing.PendingActionID}
	case ExistingRejected:
		return ClaimResult{Outcome: WriteRefused, Reason: "a human denied this action", ClaimID: existing.PendingActionID}
	case ExistingAwaitingApproval:
		return ClaimResult{Outcome: WriteNeedsApproval, Reason: "awaiting human approval", ClaimID: existing.PendingActionID}
	default:
		// ExistingNone or ExistingExpired: the row was settled or reclaimed between the
		// failed insert and this read. Reporting already-applied would claim something
		// that may not have happened, so refuse and let the client retry — the next
		// attempt will win the insert cleanly.
		return ClaimResult{
			Outcome: WriteRefused,
			Reason:  "this call raced an identical one; retry",
		}
	}
}

// SettleWrite records what became of a claimed write.
//
// MUST be called for every WriteProceed that carried a ClaimID, including the failure
// path — a claim left unsettled is read as "already applied" by every later retry, so
// forgetting to settle turns a failed write into one that is forever reported as done.
// Best-effort by design: the write itself has already happened (or not), and failing
// the caller's request because the bookkeeping update failed would misreport a
// completed write. The abandoned-claim reclaimer is the backstop.
func SettleWrite(ctx context.Context, claimID string, applied bool, result string) {
	id, err := uuid.Parse(strings.TrimSpace(claimID))
	if err != nil {
		return
	}

	status := pendingModels.StatusExecuted
	errMsg := ""
	if !applied {
		// 'failed' is read back as "no prior call", which is what frees the key for a
		// legitimate retry after a transient fault.
		status = pendingModels.StatusFailed
		errMsg = "the tool did not complete"
	}

	// resolved_by is the accountable person on the row itself: nobody approved this,
	// so attributing it to an approver would invent a decision that was never made.
	// Finalize needs a uuid, and the row's requested_by is the honest one.
	row, gerr := pendingModels.GetByID(ctx, id)
	if gerr != nil || row == nil {
		return
	}
	if ferr := pendingModels.Finalize(ctx, id, status, helpers.TruncateRunes(result, 2000), errMsg, row.RequestedBy); ferr != nil {
		// Logged loudly: an unsettled claim blocks that exact call from ever being
		// retried successfully until the reclaimer frees it.
		helpers.LogErrorWithContext(ctx,
			"business/MCPServer could not settle write claim %s (status %s); the reclaimer will free it: %+v",
			claimID, status, ferr)
	}
}

// StartPendingActionHousekeeping runs the two sweeps ai_pending_actions needs.
//
// NEITHER WAS SCHEDULED. ExpireStale has existed since the approval feature shipped and
// nothing ever called it, so overdue proposals stayed 'pending' forever — harmless for
// the open-actions list, which filters on expires_at itself, but it means a row's status
// never told the truth and an audit of "what was never decided" found nothing.
//
// The second sweep matters much more. A row in 'executing' is read as "already
// applied", and until now only the process that claimed it could move it out. A server
// killed mid-write left the key claimed forever, and from then on every retry of that
// call was told it had succeeded when it never ran. That affects the in-app approval
// path exactly as much as this one, which is why the sweep is generic housekeeping
// rather than something MCP-specific.
func StartPendingActionHousekeeping() {
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for range t.C {
			ctx := context.Background()

			if n, err := pendingModels.ExpireStale(ctx); err != nil {
				helpers.LogWarnWithContext(ctx, "pending-action expiry sweep failed: %+v", err)
			} else if n > 0 {
				helpers.LogInfoWithContext(ctx, "pending-action sweep expired %d overdue proposal(s)", n)
			}

			if n, err := pendingModels.ReclaimAbandonedExecutions(ctx); err != nil {
				helpers.LogWarnWithContext(ctx, "abandoned-execution sweep failed: %+v", err)
			} else if n > 0 {
				// Warn, not info: this means a server died mid-write, and the freed key
				// may now permit a duplicate of a write that actually landed.
				helpers.LogWarnWithContext(ctx,
					"reclaimed %d abandoned action execution(s); a retry of those calls is now permitted again", n)
			}
		}
	}()
}
