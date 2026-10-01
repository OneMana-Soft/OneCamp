package business

// MCP tool annotations, and the delegation-depth model.
//
// ANNOTATIONS. MCP defines readOnlyHint / destructiveHint / idempotentHint so a
// client can reason about a tool before calling it. We emit them because clients
// use them to decide whether to auto-retry and whether to ask a human first.
//
// We also treat them as advertising, not as protection. The specification is
// explicit that annotations are not guaranteed to describe a tool's behaviour
// faithfully and that clients must treat them as untrusted. That cuts both ways:
// OUR annotations are a promise we have to keep server-side (see
// ToolBehaviour.needsIdempotencyKey and its registration check), and any
// annotation we ever RECEIVE from an external server is untrusted input.
//
// DEPTH. A chain can now enter from outside: an external agent calls a OneCamp
// tool, and a OneCamp agent may delegate onward. The chain then spans two trust
// domains and the obvious question is how to count hops.
//
// The answer comes from how the emerging drafts model it. Attenuating-token work
// (draft-niyikiza-oauth-attenuating-agent-tokens) has a holder derive a narrower
// token "subject to the parent token's depth and lifetime limits" — depth is a
// property of the CREDENTIAL, inherited and decremented, not of the surface being
// touched. And the actor-chain work (draft-mw-oauth-actor-chain) separates
// DECLARED from VERIFIED disclosure, observing that under RFC 8693 prior actors in
// a chain "remain informational only".
//
// So:
//
//   - depth is counted from OUR credential, never from what the caller asserts
//   - a chain an external caller declares is recorded as untrusted metadata and
//     never used to make a decision
//
// This matters because the alternative is an obvious escalation: if we trusted an
// inbound "hop: 0", any caller could reset their own budget by lying about it, and
// the hop budget would protect nothing.

import (
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// Annotations is the MCP annotation set for one tool, in the protocol's own field
// names so it serialises straight into a tools/list reply.
type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
}

// AnnotationsFor derives the advertised annotations from a tool's declared
// behaviour. One direction only — behaviour is the source of truth and annotations
// are generated — so what we advertise cannot drift from what we enforce.
func AnnotationsFor(spec *ToolSpec) Annotations {
	if spec == nil {
		return Annotations{}
	}
	return Annotations{
		Title:        spec.Name,
		ReadOnlyHint: spec.Behaviour.ReadOnly,
		// A read-only tool is never destructive, whatever it declared; the
		// registration check already refuses that combination, and generating from
		// behaviour means we cannot emit the contradiction even if it slipped in.
		DestructiveHint: spec.Behaviour.Destructive && !spec.Behaviour.ReadOnly,
		// Reads are inherently repeatable, so advertise idempotency for them even
		// when a spec did not bother to say so.
		IdempotentHint: spec.Behaviour.Idempotent || spec.Behaviour.ReadOnly,
	}
}

// DeclaredChain is delegation lineage asserted by an EXTERNAL caller.
//
// Informational only, by design. It is recorded in the audit trail so a chain that
// crosses a trust boundary can still be reconstructed afterwards, and it is never
// consulted to authorise anything. The distinction is the declared-versus-verified
// split the actor-chain draft makes: we can repeat what we were told, we cannot
// vouch for it.
type DeclaredChain struct {
	// Actors are the upstream agent identifiers the caller claims acted before it.
	Actors []string
	// Hop is the depth the caller claims. Never used in a budget decision.
	Hop int
}

// EntryDepth is the hop a chain entering over MCP starts at.
//
// One, not zero. An external call is already a delegated act: some agent, somewhere,
// decided to make it. Starting at zero would give an inbound chain the full internal
// budget on top of whatever it had already spent outside, which is exactly the
// laundering the budget exists to prevent.
const EntryDepth = 1

// EffectiveDepth returns the hop count to enforce for a call arriving over MCP.
//
// Deliberately ignores declared.Hop. A caller that could raise its own budget by
// asserting a lower hop would face no budget at all, and there is no way to verify
// an assertion from outside our trust domain. The parameter exists so the caller's
// claim is passed in and visibly discarded, rather than silently unused — a reader
// should be able to see the decision, not infer it from an absence.
func EffectiveDepth(declared DeclaredChain) int {
	return EntryDepth
}

// SanitizeDeclaredActors bounds and cleans an externally-supplied chain before it is
// recorded. Untrusted input reaching an audit row still has to be safe: an
// unbounded actor list is an amplification vector into the one table we need to
// stay readable and verifiable.
func SanitizeDeclaredActors(actors []string) []string {
	const (
		maxActors   = 16
		maxActorLen = 128
	)
	out := make([]string, 0, len(actors))
	for _, a := range actors {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		// Rune-counted, not byte-sliced: an actor id is externally supplied and may
		// be any script, and a byte slice would emit invalid UTF-8 into the audit
		// chain — where the hash is computed over the content, so a malformed value
		// is permanent.
		a = helpers.TruncateRunes(a, maxActorLen)
		out = append(out, a)
		if len(out) == maxActors {
			break
		}
	}
	return out
}
