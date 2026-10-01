package business

// The write path: what happens between "this call is authorised" and "this call
// changes something".
//
// Authorization says the caller MAY act. It does not say the act should happen now,
// unreviewed, and exactly once. Three separate concerns, all of which have to be
// settled before a mutating tool runs, and all of which are settled here rather
// than in each tool — because twenty tools deciding this individually is nineteen
// chances to get it wrong.
//
// EXACTLY ONCE. MCP clients may retry: the 2026-07-28 revision made the protocol
// stateless and, for a tool needing mid-call input, has the client RESEND the
// original request. The specification also states annotations are untrusted hints,
// so a client may retry a tool regardless of what we advertised. A dropped response
// therefore becomes a duplicate write unless the server deduplicates. It does, via
// a key derived from the ARGUMENTS — never generated per call, because a fresh key
// on retry deduplicates nothing.
//
// REVIEWED. A destructive act does not execute on an agent's word. It creates a
// pending action, which OneCamp already surfaces as an in-thread Approve/Deny card
// with a TTL and live delivery, and the tool returns MRTR "input_required". The
// human approves IN ONECAMP, where the work lives, and the agent's retry finds the
// approval and proceeds.
//
// The consequence is the strong claim: two independent humans are involved in any
// destructive write — the one who authorised the credential, and the one who
// approved this specific act. That is a materially better answer to a human
// oversight obligation than "we kept a log".
//
// NOT EVERYTHING. Reads and additive writes proceed directly. Asking for approval
// on everything trains people to click Approve without reading, which is worse than
// not asking.
//
// REUSES ai_pending_actions (migration 101) and its unique partial index on
// idempotency_key. No new datastore, and no second approval model to drift from the
// one the product already shows users.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// WriteOutcome is what the transport should do with an authorised mutating call.
type WriteOutcome string

const (
	// WriteProceed — run the handler now.
	WriteProceed WriteOutcome = "proceed"
	// WriteNeedsApproval — a human must approve first. The transport answers MRTR
	// `input_required`; the client resends and finds the decision.
	WriteNeedsApproval WriteOutcome = "needs_approval"
	// WriteAlreadyApplied — a previous identical call already did this. The
	// transport answers success WITHOUT running the handler, because a retry that
	// re-executes is the duplicate this exists to prevent.
	WriteAlreadyApplied WriteOutcome = "already_applied"
	// WriteRefused — approval was explicitly denied, or the request expired.
	WriteRefused WriteOutcome = "refused"
)

// WritePlan is the decision about a single mutating call.
type WritePlan struct {
	Outcome WriteOutcome
	// Reason is always populated, so the audit row and the client message agree.
	Reason string
	// IdempotencyKey is the derived key, recorded so a duplicate is traceable to the
	// call it duplicated.
	IdempotencyKey string
	// PendingActionID identifies the approval to wait on, when Outcome is
	// WriteNeedsApproval.
	PendingActionID string
}

// idempotencyScope binds a derived key to the tool, the resource and the principal.
//
// Without scoping, two different tools deriving the same key from similar arguments
// would collide and one would be silently treated as a duplicate of the other. And
// without the principal, one person's call could suppress another's identical one —
// which is a correctness bug that looks exactly like a permission bug.
func idempotencyScope(toolName string, ref ResourceRef, principalUserID, rawKey string) string {
	h := sha256.New()
	// Length-prefixed so the parts cannot be confused by a separator appearing
	// inside one of them; "a|b" and "a" + "|b" must not hash alike.
	for _, part := range []string{toolName, string(ref.Kind), ref.ID, principalUserID, rawKey} {
		fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// DeriveIdempotencyKey computes the deduplication key for an authorised call.
//
// Returns "" with no error for a tool that needs no key (read-only, or naturally
// idempotent) — those are safe to repeat by definition, so there is nothing to
// deduplicate.
func DeriveIdempotencyKey(spec *ToolSpec, call ToolCallContext) (string, error) {
	if spec == nil {
		return "", fmt.Errorf("mcp: no tool spec")
	}
	if !spec.Behaviour.needsIdempotencyKey() {
		return "", nil
	}
	if spec.IdempotencyKey == nil {
		// Registration refuses this, so reaching it means the registry was bypassed.
		// Refusing here rather than proceeding keeps the invariant true even then.
		return "", fmt.Errorf("mcp: tool %q mutates and provides no IdempotencyKey", spec.Name)
	}
	raw, err := spec.IdempotencyKey(call.Args)
	if err != nil {
		return "", fmt.Errorf("mcp: could not derive an idempotency key for %q: %w", spec.Name, err)
	}
	if strings.TrimSpace(raw) == "" {
		// An empty key would make every call to this tool a duplicate of the first.
		return "", fmt.Errorf("mcp: tool %q derived an empty idempotency key", spec.Name)
	}
	return idempotencyScope(spec.Name, call.Resource, call.PrincipalUserID, raw), nil
}

// RequiresApproval reports whether a call needs a human to approve this specific act
// before it runs.
//
// Destructive only. A destructive tool mutates or removes durable state and is not
// made safe by calling it again, which is exactly the class where an agent's mistake
// is not self-correcting. Additive writes are recoverable and reviewable after the
// fact, and gating them too would devalue the prompt.
func RequiresApproval(spec *ToolSpec) bool {
	return spec != nil && spec.Behaviour.Destructive && !spec.Behaviour.ReadOnly
}

// ApprovalDescription is the sentence a human sees on the Approve/Deny card.
//
// Written for the REVIEWER, not the agent: what will change, to what, and who asked.
// A card that says "run tool delete_task" cannot be approved responsibly, so the
// description names the resource and the requesting agent, and is bounded because it
// is rendered in a UI and stored.
func ApprovalDescription(spec *ToolSpec, call ToolCallContext, clientName string) string {
	if spec == nil {
		return ""
	}
	client := strings.TrimSpace(clientName)
	if client == "" {
		client = "an external agent"
	}
	desc := fmt.Sprintf("%s wants to run %q on %s %s",
		client, spec.Name, call.Resource.Kind, call.Resource.ID)
	if spec.Behaviour.Destructive {
		desc += " — this changes or removes existing content and is not undone by running it again"
	}
	return helpers.TruncateRunes(desc, 500)
}

// PlanWrite decides what to do with an authorised mutating call.
//
// Split from execution deliberately: the plan is a pure-ish decision the transport
// can audit BEFORE anything changes, so a refused or deferred write is recorded with
// the same certainty as a completed one.
//
// checkExisting is injected rather than called directly so this stays testable
// without a database, and so the pending-action store can be swapped without
// touching the policy. It reports whether a prior identical call exists and what
// became of it.
func PlanWrite(
	ctx context.Context,
	spec *ToolSpec,
	call ToolCallContext,
	clientName string,
	checkExisting func(ctx context.Context, idempotencyKey string) (ExistingWrite, error),
) WritePlan {
	if spec == nil {
		return WritePlan{Outcome: WriteRefused, Reason: "no tool spec"}
	}

	// A read needs none of this.
	if spec.Behaviour.ReadOnly {
		return WritePlan{Outcome: WriteProceed, Reason: "read-only tool"}
	}

	key, err := DeriveIdempotencyKey(spec, call)
	if err != nil {
		// Refusing rather than proceeding without a key: an un-deduplicated write is
		// the failure this whole file exists to prevent, so losing the key means
		// losing the guarantee.
		return WritePlan{Outcome: WriteRefused, Reason: err.Error()}
	}

	// A naturally idempotent write needs no bookkeeping — repeating it is harmless
	// by definition.
	if key == "" {
		return WritePlan{Outcome: WriteProceed, Reason: "idempotent tool"}
	}

	if checkExisting != nil {
		existing, cerr := checkExisting(ctx, key)
		if cerr != nil {
			// Cannot tell whether this was already applied. Refuse: applying a write
			// twice is worse than not applying it, and the caller can retry once the
			// store is healthy.
			return WritePlan{
				Outcome:        WriteRefused,
				Reason:         "could not determine whether this call was already applied; refusing rather than risking a duplicate",
				IdempotencyKey: key,
			}
		}
		switch existing.State {
		case ExistingApplied:
			return WritePlan{Outcome: WriteAlreadyApplied, Reason: "an identical call was already applied", IdempotencyKey: key, PendingActionID: existing.PendingActionID}
		case ExistingAwaitingApproval:
			return WritePlan{Outcome: WriteNeedsApproval, Reason: "awaiting human approval", IdempotencyKey: key, PendingActionID: existing.PendingActionID}
		case ExistingRejected:
			return WritePlan{Outcome: WriteRefused, Reason: "a human denied this action", IdempotencyKey: key, PendingActionID: existing.PendingActionID}
		case ExistingExpired:
			// Treated as absent: the approval window closed, so asking again is the
			// correct behaviour rather than refusing forever.
		}
	}

	if RequiresApproval(spec) {
		return WritePlan{Outcome: WriteNeedsApproval, Reason: "destructive action requires human approval", IdempotencyKey: key}
	}
	return WritePlan{Outcome: WriteProceed, Reason: "additive write", IdempotencyKey: key}
}

// ExistingWriteState is what became of a previous identical call.
type ExistingWriteState string

const (
	// ExistingNone — no prior call with this key.
	ExistingNone ExistingWriteState = "none"
	// ExistingAwaitingApproval — asked, not yet decided.
	ExistingAwaitingApproval ExistingWriteState = "awaiting_approval"
	// ExistingApplied — already executed. Do not execute again.
	ExistingApplied ExistingWriteState = "applied"
	// ExistingRejected — a human said no.
	ExistingRejected ExistingWriteState = "rejected"
	// ExistingExpired — the approval window closed without a decision.
	ExistingExpired ExistingWriteState = "expired"
)

// ExistingWrite is the lookup result for an idempotency key.
type ExistingWrite struct {
	State           ExistingWriteState
	PendingActionID string
}
