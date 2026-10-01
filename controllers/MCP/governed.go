package controllers

// The governed path for /v1/mcp.
//
// WHY THIS SITS BESIDE THE EXISTING HANDLER RATHER THAN REPLACING IT. /v1/mcp
// already serves ~25 tools, several of them writes, to whatever clients customers
// have pointed at it. Re-pointing all of them at a new authorization path in one
// change would mean a single commit whose blast radius is "the entire MCP surface",
// and whose failure mode is refusals that look like outages.
//
// So the migration is per tool. A tool that has been given an authority rule — that
// is, registered in business/MCPServer — is served here. Everything else takes the
// path it took yesterday. The set is data, in bridge.go, and a test requires every
// public tool to be in one group or the other, so this is a frontier that moves
// deliberately rather than a fork that is forgotten.
//
// What a tool gains by being served here:
//
//   - A per-object authority check BEFORE its handler runs, against the live
//     permissions of the person who created the credential.
//   - A call budget, counted separately for reads and writes.
//   - An audit record of the decision, INCLUDING REFUSALS. The legacy path records
//     successful writes only, so today a burst of denied calls — which is what
//     probing looks like — leaves no trace at all.
//
// What it does not change: the tool's implementation. The same executor runs, with
// the same arguments, as the same person.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	mcpBusiness "github.com/akashc777/OneCamp/business/MCPServer"
	"github.com/akashc777/OneCamp/helpers"
)

// JSON-RPC codes for the governed outcomes.
//
// A REFUSAL IS NOT AN INTERNAL ERROR. The legacy path answers a missing scope with
// -32603 (internal error), which tells a client the server broke when in fact the
// server decided. A client cannot act sensibly on that: it retries, because that is
// what you do with a server error. These codes distinguish "your credential is
// unusable" from "you may not do this" from "slow down", because those want three
// different client behaviours.
const (
	codeGovernedUnauthorized = -32001
	codeGovernedRefused      = -32002
	codeGovernedBudget       = -32003
	codeGovernedToolFailed   = -32004
)

// isGoverned reports whether a tool has been migrated onto the governed path.
func isGoverned(tool string) bool {
	_, ok := mcpBusiness.Lookup(strings.TrimSpace(tool))
	return ok
}

// handleGovernedToolCall authorises and runs one migrated tool.
func handleGovernedToolCall(w http.ResponseWriter, r *http.Request, req jsonRPCRequest, name string, args map[string]interface{}) {
	ctx := r.Context()

	// THE CREDENTIAL IS RE-PRESENTED, not taken from the middleware's context.
	//
	// VerifyApiToken has already validated it, so this is a second lookup by hash.
	// Accepted deliberately: the authorizer owns its whole ladder, and its first
	// rung is validating the actor. If it accepted a pre-resolved identity from a
	// caller, its guarantee would weaken from "this is what the credential permits"
	// to "this is what the caller said the credential permits" — and the caller is
	// the layer whose mistakes the authorizer exists to survive. One indexed lookup
	// is a cheap price for that, and the last-used write it triggers is throttled.
	decision := mcpBusiness.AuthorizeToolCall(ctx, mcpBusiness.ToolCallRequest{
		TokenPlaintext: governedBearer(r),
		ToolName:       name,
		Args:           args,
		ClientName:     clientNameFrom(req),
		ClientVersion:  clientVersionFrom(req),
	})

	// AUDIT BEFORE ACTING, and audit refusals too.
	//
	// Written first so the record exists even if the handler then panics or the
	// process dies mid-call: a decision that was made and not recorded is worse than
	// one recorded and then abandoned, because only the second is discoverable.
	//
	// A failure to record is fatal to the call. The chain is the evidence the whole
	// surface is sold on; proceeding without it would mean acting on someone's
	// workspace with no record that it happened.
	if err := mcpBusiness.RecordDecision(ctx, decision, name, mcpBusiness.AuditContext{
		ClientName:    clientNameFrom(req),
		ClientVersion: clientVersionFrom(req),
	}); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/MCP could not record decision for %s: %+v", name, err)
		writeRPCError(w, req.ID, codeGovernedToolFailed,
			"this call could not be recorded in the audit trail and was therefore not performed")
		return
	}

	if !decision.Allow {
		writeGovernedRefusal(w, req, decision)
		return
	}

	// WHAT SHOULD HAPPEN TO A MUTATING CALL. Read-only tools fall straight through;
	// everything else is decided by policy in business/MCPServer, never here.
	//
	// The decision comes BEFORE the handler and is acted on before anything changes,
	// so a deferred or duplicate write is settled with the same certainty as a
	// completed one.
	plan := mcpBusiness.PlanWrite(ctx, decision.Spec, decision.Call,
		clientNameFrom(req), mcpBusiness.CheckExistingWrite)

	switch plan.Outcome {
	case mcpBusiness.WriteProceed:
		// Fall through and run it.

	case mcpBusiness.WriteAlreadyApplied:
		// SUCCESS WITHOUT RE-EXECUTING. This is the whole point of the idempotency
		// key: an MCP client may retry a call whose response was lost, and answering
		// that retry by doing the work again is the duplicate write the key exists to
		// prevent. Reporting success is also honest — the caller's intent HAS been
		// applied, just not by this request.
		writeRPCResult(w, req.ID, map[string]interface{}{
			"content":           []map[string]interface{}{{"type": "text", "text": plan.Reason}},
			"structuredContent": map[string]any{"applied": true, "duplicate": true},
			"isError":           false,
		})
		return

	case mcpBusiness.WriteNeedsApproval:
		handleApprovalRequired(w, r, req, decision, plan)
		return

	default:
		// WriteRefused, and anything a future outcome adds. Defaulting to a refusal
		// means a new outcome cannot accidentally execute before someone has decided
		// what it means.
		writeRPCError(w, req.ID, codeGovernedRefused, plan.Reason)
		return
	}

	// RESERVE THE CALL AGAINST DUPLICATES, for a write that is not safe to repeat.
	//
	// PlanWrite decided this call may proceed and derived its identity; nothing until
	// now wrote that identity down, so a client's retry after a lost response derived
	// the same key, found no record, and did the work twice. This is where the record
	// is made — before the handler runs, so two concurrent retries are separated by the
	// database rather than by luck.
	//
	// A no-op for reads and naturally idempotent writes: the key is empty, so
	// ClaimWrite simply says proceed. Hence no branch here on what kind of tool it is.
	claim := mcpBusiness.ClaimWrite(ctx, decision.Spec, decision.Call,
		clientNameFrom(req), plan.IdempotencyKey)
	if claim.Outcome != mcpBusiness.WriteProceed {
		// The identical call already has an answer. Reported from that answer rather
		// than re-executing, which is the entire point of having a key.
		writeClaimOutcome(w, req, claim)
		return
	}

	// BILL THE AI SPEND THIS CALL CAUSES. Several tools reach a model on the way to
	// their answer (search_workspace embeds the query; the summarisers run a
	// completion), and until this the per-agent daily cap never engaged on this
	// surface — an admin's max_daily_tokens held for in-app runs and was silently
	// ignored over MCP. Applied to every tool unconditionally, because deciding
	// per-tool whether AI is involved is a judgement that has to stay correct as
	// tools are added. See business/MCPServer/spend.go.
	out, err := decision.Spec.Handler(mcpBusiness.SpendContext(ctx, decision.Actor), decision.Call)

	// SETTLE THE RESERVATION, on both paths. An unsettled claim is read as "already
	// applied" by every later retry, so failing to settle a FAILED write would tell
	// every retry that it had succeeded. Settling a failure frees the key so a
	// transient fault stays retryable.
	if claim.ClaimID != "" {
		// The handler's output is stored on the claim row as the record of what this
		// write produced, so a duplicate found later can be compared against it. A
		// non-string result is rendered rather than dropped, since "something was
		// returned" is itself worth keeping.
		mcpBusiness.SettleWrite(ctx, claim.ClaimID, err == nil, fmt.Sprintf("%v", out))
	}

	if err != nil {
		// The handler's own message is not forwarded. It is written for an operator
		// and can name internals the caller was authorised to act on but not to be
		// told about. The audit row already carries the attribution.
		helpers.LogErrorWithContext(ctx, "controllers/MCP governed tool %s failed: %+v", name, err)
		writeRPCResult(w, req.ID, toolResult("the tool could not complete", true))
		return
	}

	writeRPCResult(w, req.ID, governedToolResult(out))
}

// writeClaimOutcome answers a call whose identity was already reserved by an earlier
// arrival of the same call.
//
// Deliberately shaped identically to the WriteAlreadyApplied reply above, because it is
// the same fact discovered a moment later: PlanWrite looks for a prior call before the
// reservation, and this catches the one that arrived between that look and the insert.
// A client must not be able to tell which of the two noticed.
func writeClaimOutcome(w http.ResponseWriter, req jsonRPCRequest, claim mcpBusiness.ClaimResult) {
	switch claim.Outcome {
	case mcpBusiness.WriteAlreadyApplied:
		writeRPCResult(w, req.ID, map[string]interface{}{
			"content":           []map[string]interface{}{{"type": "text", "text": claim.Reason}},
			"structuredContent": map[string]any{"applied": true, "duplicate": true},
			"isError":           false,
		})
	case mcpBusiness.WriteNeedsApproval:
		// An identical call is already waiting on a human. Reported with the same code
		// the approval path uses, so a client's handling of "come back later" is one
		// branch rather than two.
		writeRPCError(w, req.ID, codeGovernedRefused, claim.Reason)
	default:
		writeRPCError(w, req.ID, codeGovernedRefused, claim.Reason)
	}
}

// writeGovernedRefusal maps a refusal to the code that tells a client what to do
// about it.
func writeGovernedRefusal(w http.ResponseWriter, req jsonRPCRequest, d mcpBusiness.Decision) {
	switch {
	// Budget is the only refusal that waiting fixes, so it is the only one that
	// invites a retry. Saying so for a permission refusal would produce exactly the
	// retry loop the budget exists to stop.
	case d.RetryAfter > 0:
		w.Header().Set("Retry-After", secondsCeil(d))
		writeRPCError(w, req.ID, codeGovernedBudget, d.Reason)
	// No token id means nothing could be attributed: the credential itself is
	// unusable. Distinct from a permission answer, because a client should respond
	// by fixing its credential rather than by giving up on the tool.
	case strings.TrimSpace(d.TokenID) == "":
		writeRPCError(w, req.ID, codeGovernedUnauthorized, "a valid workspace API token is required")
	default:
		writeRPCError(w, req.ID, codeGovernedRefused, d.Reason)
	}
}

// governedToolResult renders a handler's output in MCP's tool-result shape.
//
// The text block is what clients that only render text will show; structuredContent
// is what a client should actually read. Both come from the same value, so they
// cannot describe different results.
func governedToolResult(out any) map[string]interface{} {
	text := ""
	if m, ok := out.(map[string]any); ok {
		if s, ok := m["text"].(string); ok {
			text = s
		}
	}
	return map[string]interface{}{
		"content":           []map[string]interface{}{{"type": "text", "text": text}},
		"structuredContent": out,
		"isError":           false,
	}
}

// governedBearer re-reads the credential from the request.
func governedBearer(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) >= 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// secondsCeil renders Retry-After, rounding up so a client never returns early and
// is immediately refused again.
func secondsCeil(d mcpBusiness.Decision) string {
	secs := int(d.RetryAfter.Seconds())
	if float64(secs) < d.RetryAfter.Seconds() {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	return strings.TrimSpace(itoaSmall(secs))
}

func itoaSmall(n int) string {
	if n <= 0 {
		return "1"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// clientNameFrom / clientVersionFrom read the caller's self-description.
//
// Untrusted, and recorded anyway: "which agent software did this" is worth being
// able to answer approximately, and it is never consulted to decide anything. Under
// the current handshake era this arrives in initialize; the stateless revision moves
// it to per-request _meta, which is why it is read from the request rather than from
// remembered session state.
func clientNameFrom(req jsonRPCRequest) string    { return metaString(req, "name") }
func clientVersionFrom(req jsonRPCRequest) string { return metaString(req, "version") }

func metaString(req jsonRPCRequest, field string) string {
	if len(req.Meta) == 0 {
		return ""
	}
	var meta struct {
		ClientInfo map[string]string `json:"clientInfo"`
	}
	if err := json.Unmarshal(req.Meta, &meta); err != nil {
		return ""
	}
	return strings.TrimSpace(meta.ClientInfo[field])
}

// handleApprovalRequired parks a destructive write behind the in-thread Approve/Deny
// card and tells the client to come back.
//
// THE CARD IS THE ONE THE APP ALREADY USES. A reviewer sees an external agent's
// proposal exactly as they see a colleague's, with the requesting client named in the
// description. They do not need to know it arrived over MCP, which is the point:
// approving agent work should not require understanding agent plumbing.
//
// TWO INDEPENDENT PEOPLE, WITHOUT EITHER STEP KNOWING ABOUT THE OTHER. The principal
// authorised the credential; a second human approves this specific action from the
// surface. This caller cannot be that second human — it is an external process with no
// session — so the separation holds by construction rather than by a rule someone must
// enforce.
func handleApprovalRequired(w http.ResponseWriter, r *http.Request, req jsonRPCRequest,
	decision mcpBusiness.Decision, plan mcpBusiness.WritePlan) {
	ctx := r.Context()

	pendingID := plan.PendingActionID
	if strings.TrimSpace(pendingID) == "" {
		// First time this call has been seen: raise the card. A key already awaiting
		// a decision arrives here with an id set, so this creates one card per logical
		// write rather than one per retry.
		id, err := mcpBusiness.RequestApproval(ctx, decision.Spec, decision.Call,
			clientNameFrom(req), plan.IdempotencyKey)
		if err != nil {
			// Could not raise the card, so nobody can approve it — and proceeding
			// would be executing the destructive write this branch exists to hold
			// back. Refuse.
			helpers.LogErrorWithContext(ctx, "controllers/MCP could not raise approval for %s: %+v",
				decision.Spec.Name, err)
			writeRPCError(w, req.ID, codeGovernedRefused,
				"this action needs a person to approve it and the approval could not be raised")
			return
		}
		pendingID = id
	}

	// A DEFERRAL IS NOT AN ERROR AND NOT A SUCCESS, and saying so precisely is what
	// lets a client behave sensibly. Retry-After tells it when to look again rather
	// than leaving it to poll; the pending id lets a human be pointed at the exact
	// card. Nothing here says the write happened, because it has not.
	w.Header().Set("Retry-After", approvalRetryAfterSeconds)
	writeRPCResult(w, req.ID, map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text",
			"text": "This action changes or removes existing content, so a person in the " +
				"workspace has been asked to approve it. Retry later to find out what they decided."}},
		"structuredContent": map[string]any{
			"status":            "input_required",
			"pending_action_id": pendingID,
			"reason":            plan.Reason,
		},
		// NOT isError. An error invites a client to retry immediately or give up; this
		// is neither outcome. The stateless revision's MRTR shape says the same thing
		// in the protocol's own vocabulary, and this is its equivalent under the
		// handshake era.
		"isError": false,
	})
}

// approvalRetryAfterSeconds is how long a client should wait before asking again.
//
// Sized to a human noticing a card rather than to a machine's patience: a minute is
// short enough that a client is not stalled and long enough that retrying is not a
// poll. The approval itself has its own TTL in the store, so a client that gives up
// entirely loses nothing that was not going to expire.
const approvalRetryAfterSeconds = "60"
