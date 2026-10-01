package business

// Per-credential call budgets.
//
// WHY READS AND WRITES ARE BUDGETED APART. A runaway read loop wastes cycles. A
// runaway write loop puts a thousand messages in a channel, and no amount of
// after-the-fact auditing un-sends those. They are different failure severities, so
// they get different buckets and very different caps.
//
// WHY NOT REUSE THE /v1 TOKEN LIMIT. A human-written integration makes a predictable
// number of calls; an agent loops, and looping is its NORMAL failure mode rather than
// an exceptional one. Sharing a bucket would let a misbehaving agent exhaust the
// budget a person's integration depends on. Separate namespaces, separate blast
// radius.
//
// FAIL OPEN, DELIBERATELY, AND ONLY HERE. Every other guard in this package fails
// closed. This one does not: if Redis is unavailable the call proceeds. That
// asymmetry is reasoned, not lazy — a budget protects against VOLUME, and the
// authorization and audit guards protect against everything that actually matters.
// Refusing all agent traffic because a cache is down would convert a cache outage
// into a product outage, while permitting it risks only that someone briefly exceeds
// a rate limit. The existing /v1 limiter makes the same choice for the same reason.
//
// The distinction worth holding on to: fail closed on QUESTIONS OF AUTHORITY, fail
// open on QUESTIONS OF VOLUME.

import (
	"context"
	"time"

	// Aliased: this package already has a `registry` of TOOLS, and two different
	// registries under one name is how a reader ends up debugging the wrong thing.
	rateRegistry "github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// Default caps per credential per minute.
//
// Reads are generous because answering one question legitimately fans out over many
// documents. Writes are low enough that a person notices and intervenes before a
// mistake becomes interesting — which is the only control that works against a
// failure nobody predicted.
const (
	DefaultReadCallsPerMinute  = 240
	DefaultWriteCallsPerMinute = 30
)

// BudgetDecision is the outcome of a budget check.
type BudgetDecision struct {
	Allow bool
	// Reason is always populated, so the audit record and the client message agree.
	Reason string
	// RetryAfter tells a well-behaved client when to come back. Zero when allowed.
	RetryAfter time.Duration
	// Count and Limit are recorded so a refusal shows how far over it went — the
	// difference between "just over" and "looping" is what an operator needs.
	Count int64
	Limit int
}

// BudgetLimits are the caps to enforce. Zero values mean the defaults, so a caller
// may pass an empty struct and get the safe behaviour.
type BudgetLimits struct {
	ReadPerMinute  int
	WritePerMinute int
}

func (b BudgetLimits) read() int {
	if b.ReadPerMinute > 0 {
		return b.ReadPerMinute
	}
	return DefaultReadCallsPerMinute
}

func (b BudgetLimits) write() int {
	if b.WritePerMinute > 0 {
		return b.WritePerMinute
	}
	return DefaultWriteCallsPerMinute
}

// CheckBudget consumes one unit of the appropriate budget for a call.
//
// Takes the tool's behaviour rather than a boolean so the caller cannot accidentally
// charge a write against the read bucket — the spec is the single source of truth for
// what a tool does, here as everywhere else in this package.
//
// NOT idempotent: it increments. So it belongs at the END of the authorization ladder,
// after every free check has passed. Charging a call that was going to be refused for
// lacking a scope would let an unauthorised caller exhaust a legitimate one's budget,
// which is a denial-of-service disguised as a rate limit.
func CheckBudget(ctx context.Context, tokenID string, spec *ToolSpec, limits BudgetLimits) BudgetDecision {
	if spec == nil {
		return BudgetDecision{Reason: "no tool spec"}
	}
	return CheckCallBudget(ctx, tokenID, spec.Behaviour.ReadOnly, limits)
}

// CheckCallBudget is the same charge on a bare read/write flag, for the public tools
// that have no ToolSpec yet and were consequently being charged against no budget at
// all.
//
// THE FLAG MUST COME FROM A REGISTRY, never from the calling code's own reading of
// what a tool does. CheckBudget takes a spec precisely so a caller cannot charge a
// write against the generous read bucket, and that protection has to survive here:
// the only intended callers pass services/AI.ToolIsReadOnly(name), which is the same
// registry that decides whether the executor mutates anything. Passing a hand-written
// boolean would reintroduce exactly the mistake the spec argument prevents.
func CheckCallBudget(ctx context.Context, tokenID string, readOnly bool, limits BudgetLimits) BudgetDecision {
	if tokenID == "" {
		// No credential to charge. Refusing here rather than allowing an uncharged
		// call: an unattributable call is exactly what a budget cannot bound.
		return BudgetDecision{Reason: "no credential to charge the budget against"}
	}

	limitSpec := rateRegistry.MCPToolWriteRate
	max := limits.write()
	kind := "write"
	if readOnly {
		limitSpec = rateRegistry.MCPToolReadRate
		max = limits.read()
		kind = "read"
	}

	res := redisStore.AllowFixedWindow(ctx, limitSpec, []string{tokenID}, max)
	if !res.Allowed {
		return BudgetDecision{
			Reason:     kind + " call budget exhausted for this credential",
			RetryAfter: res.RetryAfter,
			Count:      res.Count,
			Limit:      res.Limit,
		}
	}
	return BudgetDecision{
		Allow:  true,
		Reason: "within " + kind + " budget",
		Count:  res.Count,
		Limit:  res.Limit,
	}
}
