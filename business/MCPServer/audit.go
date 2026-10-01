package business

// Recording every MCP decision — allows AND refusals — into the hash-chained audit
// trail.
//
// WHY REFUSALS ARE THE VALUABLE HALF. Most systems log what happened. The sentence
// an auditor or an incident review actually needs is the one about what was
// PREVENTED: "your agent tried to read #board-private on Tuesday and was stopped,
// because the person who authorised it is not a member of that channel." That
// sentence is evidence of a working control. A system that only records successes
// can show that nothing bad is known to have happened, which is a much weaker claim.
//
// It is also the claim a gateway product cannot make. A gateway in front of SaaS
// sees a tool call and a token; it cannot say whose membership was missing, because
// the system behind it does not expose that.
//
// NO RECORD, NO ACTION. RecordDecision is synchronous and its error is returned to
// the caller, which must refuse the call when it fails. That is stricter than the
// rest of the product — admin audit writes are fire-and-forget, because an admin
// action is already visible in the UI even if its row is lost. An agent action
// recorded nowhere is an action nobody can review, and under logging obligations for
// high-risk AI systems (EU AI Act Article 12, applying to most standalone high-risk
// systems from 2 December 2027) an unrecorded action is a worse outcome than a
// refused one. So a failure to record has to fail the call.
//
// The entry is written to the existing SHA-256 hash chain
// (models/postgres/AdminAudit, migration 106), which is serialised under a Postgres
// advisory lock. That gives tamper evidence for free, and tamper evidence is
// precisely what the emerging delegated-authorization drafts ask an enforcement
// point to produce.

import (
	"context"
	"fmt"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// Audit action names. Two values, not one per tool: the tool is a field, so a
// reviewer can ask "show me every refusal" without enumerating the catalogue.
const (
	ActionToolCallAllowed = "mcp.tool_call.allowed"
	ActionToolCallRefused = "mcp.tool_call.refused"
)

// maxAuditFieldRunes bounds each free-text field written into the chain. The hash is
// computed over the content, so an oversized or malformed value is permanent —
// there is no editing a row out of a hash chain without breaking it.
const maxAuditFieldRunes = 512

// AuditContext is the surrounding detail a decision alone does not carry: which
// client software is calling, and what delegation lineage it claimed.
type AuditContext struct {
	// ClientName / ClientVersion come from the MCP initialize handshake. Untrusted
	// self-description, recorded because "which agent software did this" is a
	// question worth being able to answer even approximately.
	ClientName    string
	ClientVersion string
	// Declared is lineage asserted by the caller. Recorded, never trusted — see
	// EffectiveDepth.
	Declared DeclaredChain
}

// RecordDecision writes one decision to the audit chain and returns any error so
// the caller can refuse the call.
//
// Safe to call for a refusal in which no identity could be resolved: the fields are
// simply empty, and an unattributable refusal is still worth recording — a burst of
// them is what a credential-stuffing attempt looks like.
func RecordDecision(ctx context.Context, d Decision, tool string, ac AuditContext) error {
	ctx = decisionContext(ctx)
	action := ActionToolCallRefused
	if d.Allow {
		action = ActionToolCallAllowed
	}

	// The principal is the accountable human. Parsed rather than passed as a string
	// so a malformed id becomes an absent actor instead of a corrupt row.
	var actorID *uuid.UUID
	if id, err := uuid.Parse(d.PrincipalUserID); err == nil {
		actorID = &id
	}

	meta := map[string]interface{}{
		// Actor TYPE, recorded rather than inferred. A human, an integration script
		// and an autonomous agent can all reach the same endpoint and leave very
		// different risk signals; writing the type down makes "everything an agent
		// did last week" a query instead of a reconstruction.
		"actor_type": string(d.Actor.Type),
		// Actor: which credential. The id and nothing else — never the secret, and
		// never a prefix long enough to help someone guess it.
		"token_id": clip(d.TokenID),
		// Principal: which human authorised this agent. The Article 14 answer.
		"principal_user_id": clip(d.PrincipalUserID),
		"tool":              clip(tool),
		"decision":          decisionWord(d.Allow),
		// Always populated, including on allow, so a reviewer never has to infer why.
		"reason":         clip(d.Reason),
		"client_name":    clip(ac.ClientName),
		"client_version": clip(ac.ClientVersion),
		// Depth we ENFORCED, and depth the caller CLAIMED, side by side. Recording
		// both is what makes a lying caller visible after the fact rather than merely
		// unsuccessful.
		"enforced_depth": EffectiveDepth(ac.Declared),
		"declared_depth": ac.Declared.Hop,
	}
	// The named agent, when the credential is bound to one. This is the difference
	// between an audit row a reviewer can act on and a hex string they cannot: NHI
	// guidance is explicit that a record showing only a token is too weak to
	// establish intent, ownership or containment.
	if d.Actor.Type == ActorAgent {
		meta["agent_id"] = clip(d.Actor.AgentID)
		meta["agent_name"] = clip(d.Actor.AgentName)
	}
	if actors := SanitizeDeclaredActors(ac.Declared.Actors); len(actors) > 0 {
		meta["declared_actors"] = actors
	}
	// Resource is known only once the tool resolved it, so it is absent on an early
	// refusal. Omitted rather than blank, so "we never got that far" is
	// distinguishable from "it touched nothing".
	if d.Allow {
		meta["resource_kind"] = string(d.Call.Resource.Kind)
		meta["resource_id"] = clip(d.Call.Resource.ID)
	}

	// Named actor first, so the one-line summary reads as a sentence about someone
	// rather than about a token: "refused the triage agent, delete_task: ...".
	summary := fmt.Sprintf("%s %s, %s: %s", decisionWord(d.Allow), d.Actor.Label(), tool, d.Reason)

	if err := auditBusiness.RecordForPrincipal(
		ctx, actorID, "", auditActorKind(d.Actor.Type), action, auditBusiness.CategoryAgent,
		helpers.TruncateRunes(summary, maxAuditFieldRunes), meta,
	); err != nil {
		// Logged as well as returned: the caller will refuse the call, and the
		// operator needs to know the refusal was caused by the audit store rather
		// than by a permission decision.
		helpers.LogErrorWithContext(ctx,
			"MCPServer/RecordDecision failed to write the audit entry, so the call must be refused: %+v", err)
		return err
	}
	return nil
}

// decisionContext says who started an inbound call: nobody in this room.
//
// A call arriving over MCP is already a delegated act. EntryDepth is one, not
// zero, for exactly that reason: some agent, somewhere, decided to make it,
// and whether a person was watching that agent is not something this side can
// see. The honest answer the vocabulary has for that is a handoff, and it is
// the answer that lands the row under "nobody watching", where an auditor
// looking for what external tooling did on a member's authority will find it.
// A caller that already knows better keeps its own answer.
func decisionContext(ctx context.Context) context.Context {
	if _, already := auditBusiness.InitiatorFromCtx(ctx); already {
		return ctx
	}
	return auditBusiness.WithInitiator(ctx, auditBusiness.InitiatorHandoff)
}

// auditActorKind translates this package's actor type into the audit log's.
//
// An api_client is NOT an agent. It is an unbound integration credential, a
// script somebody wrote, and recording it as an agent would put the wrong
// answer in the one field a reviewer uses to separate autonomous action from
// automation somebody wrote by hand. It is not a human either, so it is system.
func auditActorKind(t ActorType) string {
	if t == ActorAgent {
		return auditBusiness.ActorAgent
	}
	return auditBusiness.ActorSystem
}

// decisionWord keeps the audit vocabulary fixed, so a query for refusals is a query
// for one string rather than a guess at phrasing.
func decisionWord(allow bool) string {
	if allow {
		return "allowed"
	}
	return "refused"
}

// clip bounds one free-text field on a rune boundary. Every value here can be
// externally influenced, and the hash chain makes whatever is written permanent.
func clip(s string) string { return helpers.TruncateRunes(s, maxAuditFieldRunes) }
