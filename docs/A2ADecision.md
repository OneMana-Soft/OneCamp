# Decision: OneCamp will not implement A2A (Agent2Agent)

**Status:** decided, implemented (the partial A2A card builder has been removed)
**Date:** 2026-07-31

## The question

A2A is the protocol agents use to discover each other and delegate across
ownership boundaries. Adopting it has two independent halves:

1. **Publish** an Agent Card per teammate — a discovery document naming the
   agent, its skills, its endpoint and its expected authentication.
2. **Accept inbound work** from external agents that found the card.

A card builder plus an admin-only preview endpoint existed. Neither half was
shipped. This records why neither will be, and why the builder was removed rather
than left in place.

## What OneCamp is

The decision follows from the product, not from the protocol's merits.

- **Self-hosted, single-tenant.** All AI configuration is global to one
  deployment. There is no multi-tenant fabric for agents to federate across.
- **Data residency is a headline feature**, not a checkbox: a dedicated spec, a
  local-only mode that pins inference to models on your own hardware, PII
  redaction, and an egress guard.
- **Governance is the differentiator.** Every agent action is attributable and
  the audit log is cryptographically verifiable and exportable.
- Positioned explicitly as the anti-SaaS: people choose it so their workspace is
  not reachable by anyone else's infrastructure.

## Why not publish cards

A published card enumerates an agent's skills, and those skills are derived from
its actually-enabled tools. That composite is a map of how a company operates:
which systems it connects, what its agents are called, what domains they act on.

Publishing it re-creates exactly the external attack and inference surface that
self-hosting exists to remove. It would be selling against the reason the product
was chosen.

## Why not accept inbound work

This is the harder objection, and it is structural rather than a matter of effort.

Every authorisation path in the agent stack answers one question: *could the
originating person have done this themselves?* That is the invariant behind
`AuthorizeDelegation` — a delegated run may never reach an agent the requesting
member could not have addressed directly — and it is what makes agent delegation
safe to switch on at all.

An external agent has no member principal. Accepting inbound work means inventing
a synthetic one, and at that moment the question becomes unanswerable: you cannot
check whether a requester could have seen a channel when the requester is not in
the workspace. Permissions would degrade from "derived from a real member's
access" to "whatever scopes we chose to mint", for a caller outside the trust
boundary.

That is not a missing feature. It contradicts the model the rest of the agent
stack is built on.

## What already serves the same need, better

The real demand — "let another system get work out of OneCamp's agents" — is
already met twice over, by paths that have what A2A lacks:

- **`/v1` with scoped API tokens.** `VerifyApiToken`, per-route `RequireScope`,
  rate limited. A token belongs to a member, so the permission question stays
  answerable.
- **`/v1/mcp` — OneCamp as an MCP server.** A JSON-RPC endpoint exposing the
  native workspace tools to any MCP-capable client, with per-tool scope checks, so
  a token only sees the tools its scopes allow.

Both land in the audit log with a real principal. A2A would add a second, weaker
door to the same room.

## Why the code was removed rather than kept

429 lines with six tests and no consumer, whose own comments described a feature
the product will not ship. Dead code that looks like a shipped capability is how
someone later exposes it: serving the card publicly was a two-line route change
away, and the preview endpoint gave the impression the decision had already been
made in favour.

Removed: `business/AIAgent/a2aCard.go`, its test, the `GetAgentCardPreview`
handler, its route, and `services/AI.ToolDescriptionFor` (added solely to feed a
card, no other caller). Git history preserves all of it if this is ever revisited.

## What would change this

Adopting A2A would need a real answer to the principal problem — most plausibly a
first-class *external agent identity* that an admin provisions, grants explicit
scopes to, and can revoke, with its own audit trail, so that inbound work has a
principal whose access can actually be evaluated. That is a substantial feature in
its own right, not a protocol adapter. Until it exists, MCP over a scoped token is
the supported answer.
