// Package controllers (MCP server) exposes OneCamp itself AS a Model Context
// Protocol server, so any MCP-capable client (Claude Desktop, Cursor, a custom
// agent) can list and call OneCamp's native workspace tools (tasks, projects,
// docs, messages, calendar, tables) over a single HTTP endpoint.
//
// Transport: streamable-HTTP MCP — a single POST endpoint that accepts a
// JSON-RPC 2.0 request and returns a JSON-RPC 2.0 response. Auth is a scoped
// API token (Authorization: Bearer oc_...), validated by the VerifyApiToken
// middleware, so every call runs AS the token's owner, narrowed to the token's
// scopes. A tool the token lacks the scope for is hidden from tools/list and
// rejected by tools/call. This reuses the exact executors the in-app AI uses,
// so the MCP surface can never do more than the user could do by hand.
package controllers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	mcpBusiness "github.com/akashc777/OneCamp/business/MCPServer"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	serverName    = "onecamp"
	serverVersion = "1.0.0"
)

// jsonRPCRequest is an incoming JSON-RPC 2.0 request. Id is kept as raw JSON so
// we echo back exactly what the client sent (number or string), per spec.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	// Meta is the per-request `_meta` object. Ignored by the handshake-era methods
	// below, and read by the governed path for the caller's self-description. The
	// 2026-07-28 revision moves protocol version and client identity here, so
	// accepting it now is what lets a modern client be served without a second
	// request shape.
	Meta json.RawMessage `json:"_meta,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// mcpTool is a tool description in MCP's tools/list shape.
//
// Annotations is a pointer so it is OMITTED for a tool whose behaviour we have not declared.
// That distinction matters: annotations tell a client whether it may run something without
// asking, so the difference between "we say this is read-only" and "we have not said" must
// survive to the wire. A zero-valued Annotations would read as an assertion that the tool is
// not read-only, not idempotent and not destructive, which is a claim we have not earned.
type mcpTool struct {
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	InputSchema interface{}              `json:"inputSchema"`
	Annotations *mcpBusiness.Annotations `json:"annotations,omitempty"`
}

// HandleRPC is the single MCP endpoint. It dispatches by JSON-RPC method.
func HandleRPC(w http.ResponseWriter, r *http.Request) {
	var req jsonRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPCError(w, req.ID, -32600, "invalid request: jsonrpc must be 2.0")
		return
	}

	// A notification (no id) is acknowledged with 202 and no body, as the Streamable HTTP
	// transport requires. Answering it with a JSON-RPC result, as this did, hands the client a
	// response to a request it never made, which the reference SDKs treat as an error.
	if len(req.ID) == 0 || strings.HasPrefix(req.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		writeRPCResult(w, req.ID, map[string]interface{}{
			"protocolVersion": mcpBusiness.NegotiateProtocolVersion(params.ProtocolVersion),
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": serverName, "version": serverVersion},
		})
	case "ping":
		writeRPCResult(w, req.ID, map[string]interface{}{})
	case "tools/list":
		writeRPCResult(w, req.ID, map[string]interface{}{"tools": listToolsForScopes(r)})
	case "tools/call":
		handleToolCall(w, r, req)
	default:
		writeRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// listToolsForScopes returns the publicly-exposable native tools the token's scopes
// permit AND the workspace has agreed to expose, in MCP schema shape.
//
// ADMISSION IS APPLIED HERE TOO, not only on the call. A catalogue that advertises
// what a call will refuse is worse than useless to an agent: it will try the tool,
// spending its budget to learn something the listing could have told it. It also stops
// a tool name disclosing that this workspace has a capability an admin deliberately
// chose not to expose.
//
// A settings read failure yields an EMPTY catalogue, matching the call path's refusal.
// The admin's decision is unknown, and the only safe reading of an unknown admission
// decision is that the surface is shut — advertising everything on a database blip is
// the one outcome that cannot be walked back.
func listToolsForScopes(r *http.Request) []mcpTool {
	granted := helpers.GetApiScopes(r.Context())
	admission, _, aerr := mcpBusiness.Admission(r.Context())
	if aerr != nil {
		return []mcpTool{}
	}

	// The ACTOR, so a credential bound to an agent sees only that agent's declared
	// toolset — and so a deactivated agent's kill switch reaches discovery, not just
	// execution. An unresolvable identity lists nothing, matching what a call would do.
	actor, actorOK, _ := mcpBusiness.ResolveActorForRequest(r.Context(), governedBearer(r))
	if !actorOK {
		return []mcpTool{}
	}

	out := make([]mcpTool, 0, len(ai.ToolRegistry))
	for _, t := range ai.ToolRegistry {
		scope, public := apiTokenBusiness.ScopeForTool(t.Name)
		if !public || !apiTokenBusiness.HasScope(granted, scope) {
			continue
		}
		if !mcpBusiness.CheckAdmissionForScope(admission, scope).Allow {
			continue
		}
		if allowed, _ := mcpBusiness.CheckAgentToolScope(actor, t.Name); !allowed {
			continue
		}
		entry := mcpTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: inputSchemaFor(t),
		}
		// ANNOTATIONS, FOR THE TOOLS WHOSE BEHAVIOUR WE HAVE WRITTEN DOWN.
		//
		// readOnlyHint / destructiveHint / idempotentHint are how a client decides whether to
		// run a call straight away or put it in front of a person. Without them a client has
		// two choices, and both are bad: prompt for every read, or auto-run every write.
		//
		// Derived from the governed registry's declared behaviour, so the hint cannot disagree
		// with what the authorizer enforces — AnnotationsFor generates in one direction only.
		//
		// Lookup finds a spec only for tools that have been migrated onto the governed path
		// (see handleToolCall: governance is a frontier, not a fork). For the rest we emit
		// nothing rather than guess, which leaves the client treating them conservatively. That
		// is the safe direction, and it is honest: we advertise a behaviour only where somebody
		// has actually declared one.
		if spec, ok := mcpBusiness.Lookup(t.Name); ok {
			annotations := mcpBusiness.AnnotationsFor(spec)
			entry.Annotations = &annotations
		}
		out = append(out, entry)
	}
	return out
}

// inputSchemaFor builds a JSON Schema object for a tool's parameters.
func inputSchemaFor(t ai.ToolDef) map[string]interface{} {
	props := map[string]interface{}{}
	required := []string{}
	for _, p := range t.Parameters {
		jsType := "string"
		if p.Type == "boolean" {
			jsType = "boolean"
		}
		props[p.Name] = map[string]interface{}{"type": jsType, "description": p.Description}
		if p.Required {
			required = append(required, p.Name)
		}
	}
	schema := map[string]interface{}{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// handleToolCall validates scope and runs the tool via the shared executor
// registry, returning the result as MCP text content.
func handleToolCall(w http.ResponseWriter, r *http.Request, req jsonRPCRequest) {
	var params struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
		writeRPCError(w, req.ID, -32602, "invalid params: name is required")
		return
	}

	// MIGRATED TOOLS GO THE GOVERNED WAY. Checked before the scope gate below
	// because the governed path performs that same check itself, as one rung of a
	// ladder that also asks about the specific object, the budget, and the audit
	// trail. Running the weaker check first would decide nothing and could refuse a
	// call the full ladder would have refused for a better-stated reason.
	//
	// A tool is governed only once someone has written down what authority over it
	// means (see business/MCPServer/bridge.go). Everything else falls through
	// unchanged, so this is a frontier, not a fork.
	if isGoverned(params.Name) {
		handleGovernedToolCall(w, r, req, params.Name, params.Arguments)
		return
	}

	// THE SURFACE GATES STILL APPLY TO AN UNGOVERNED TOOL.
	//
	// Not having an authority rule yet means the per-object check cannot run. It never
	// meant the workspace-level controls should be skipped, and they were: an admin
	// could disable the MCP surface, or enable only some tool groups, and these tools
	// kept serving; a deactivated agent's credential kept working; nothing charged a
	// call budget; and the executor ran unattributed, so the per-agent token cap could
	// not engage. All four are answerable without an authority rule.
	//
	// See business/MCPServer/ungoverned.go for the ladder and why it is deliberately a
	// different function from the governed one.
	decision := mcpBusiness.AuthorizeUngovernedCall(r.Context(), mcpBusiness.UngovernedCallRequest{
		TokenPlaintext: governedBearer(r),
		ToolName:       params.Name,
		Args:           params.Arguments,
	})

	// AUDIT BEFORE ACTING, refusals included. Same rule as the governed path: a
	// decision that was made and not recorded is worse than one recorded and then
	// abandoned, because only the second is discoverable.
	auditBusiness.Record(r, "mcp."+params.Name, auditBusiness.CategoryIntegration,
		mcpAuditSummary(params.Name, decision), map[string]interface{}{
			"token_id":  decision.TokenID,
			"actor":     string(decision.Actor.Type),
			"agent_id":  decision.Actor.AgentID,
			"allowed":   decision.Allow,
			"reason":    decision.Reason,
			"governed":  false,
			"read_only": ai.ToolIsReadOnly(params.Name),
		})

	if !decision.Allow {
		writeRPCError(w, req.ID, -32603, decision.Reason)
		return
	}

	executor, ok := ai.GetExecutor(params.Name)
	if !ok {
		writeRPCError(w, req.ID, -32601, "unknown tool: "+params.Name)
		return
	}

	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	userUUID := userInfo.UserPostgresInfo.Id.String()

	// Attribute the AI spend this call causes, so an agent-bound credential is charged
	// against the agent's own daily token cap here exactly as it is on the governed
	// path and on an in-app run.
	ctx := mcpBusiness.SpendContext(r.Context(), decision.Actor)

	action := ai.ProposedAction{ToolName: params.Name, Params: stringifyArgs(params.Arguments)}
	msg, _, err := executor(ctx, action, userUUID)
	if err != nil {
		// Tool errors are returned as a successful JSON-RPC response with
		// isError=true, per the MCP spec, so the client can surface them.
		writeRPCResult(w, req.ID, toolResult(err.Error(), true))
		return
	}
	writeRPCResult(w, req.ID, toolResult(msg, false))
}

// mcpAuditSummary is the one-line description an audit reader sees. Written for a
// person: it names the actor and says what happened, rather than restating the tool
// name a structured field already carries.
func mcpAuditSummary(tool string, d mcpBusiness.UngovernedDecision) string {
	if d.Allow {
		return "MCP tool call: " + tool + " by " + d.Actor.Label()
	}
	return "MCP tool call refused: " + tool + " by " + d.Actor.Label() + " — " + d.Reason
}

// stringifyArgs coerces JSON argument values to the string map executors take.
func stringifyArgs(in map[string]interface{}) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		switch val := v.(type) {
		case string:
			out[k] = val
		case bool:
			out[k] = fmt.Sprintf("%t", val)
		case float64:
			// JSON numbers decode to float64; render integers without a decimal.
			if val == float64(int64(val)) {
				out[k] = fmt.Sprintf("%d", int64(val))
			} else {
				out[k] = fmt.Sprintf("%g", val)
			}
		case nil:
			out[k] = ""
		default:
			b, _ := json.Marshal(val)
			out[k] = string(b)
		}
	}
	return out
}

func toolResult(text string, isErr bool) map[string]interface{} {
	return map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result interface{}) {
	helpers.WriteJSON(w, http.StatusOK, jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	helpers.WriteJSON(w, http.StatusOK, jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: &jsonRPCError{Code: code, Message: message}})
}
