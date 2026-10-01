package business

// Protocol-level values shared by the MCP surface.
//
// WHAT USED TO BE HERE, AND WHY IT IS GONE. This file held a protocol layer for a server that was
// never built: a Discover()/server-discover reply, a ServerCapabilities set, a ListTools()
// catalogue, and a SupportsProtocolVersion() gate, all built around
//
//	ProtocolVersion = "2026-07-28"
//
// Nothing called any of it, and the design it documented was the opposite of what ships. Its
// header explained at length that the server is "modern-only" and stateless — no initialize
// handshake, no session id, protocol version carried in per-request _meta — and justified that
// with a CVE identifier. The served endpoint, controllers/MCP.HandleRPC, implements `initialize`
// and answers 2024-11-05, which is a revision that actually exists and is why real clients can
// connect at all.
//
// So the file was not merely unreachable; it was a design document written in the present tense
// for a system that works differently, which is the kind of thing a reader trusts. MCP revisions
// are real dated identifiers (2024-11-05, 2025-03-26, 2025-06-18, 2025-11-25) and are an
// enumerated set rather than an ordered scale; 2026-07-28 is not among them, `server/discover` is
// not an MCP method — capability negotiation happens in `initialize` — and the CVE reference could
// not be verified. None of it was load-bearing, so all of it was removed rather than corrected.
//
// ListTools went with it for a separate and more concrete reason: it listed only the tools in the
// governed spec registry, while the endpoint serves ai.ToolRegistry and routes each call to either
// the governed or the ungoverned ladder (see handleToolCall — governance is a frontier, not a
// fork). Wiring it would have hidden every tool not yet migrated, so the controller's own loop is
// the correct implementation and is now the only one. The part of it worth keeping — MCP
// annotations derived from declared behaviour — was moved onto that loop instead.

import "errors"

// ProtocolVersion is the MCP revision the served endpoint answers in `initialize`.
//
// EXPORTED AND LIVING HERE so there is one copy. It was a private const in
// controllers/MCP, which was fine while the controller was the only thing that needed it — then the
// admin UI began printing "protocol 2024-11-05" next to the endpoint it tells operators to connect
// to, and docs/MCPServer.md had been stating it in prose all along. Three copies of a protocol
// identifier, none of which would notice the others changing.
//
// A dated identifier from an enumerated set (2024-11-05, 2025-03-26, 2025-06-18, 2025-11-25), not a
// version to bump for tidiness: it names the revision whose semantics this endpoint actually
// implements. TestTheMCPDocQuotesTheServedProtocolVersion holds the document to it.
//
// Distinct from business/AIMCP's protocolVersion, which is what OneCamp advertises as a CLIENT of
// somebody else's MCP server. Opposite direction, independently chosen.
const ProtocolVersion = "2025-06-18"

// SupportedProtocolVersions are the revisions `initialize` will agree to, newest first. A client
// asking for one of these gets it back; any other request gets ProtocolVersion, and the client
// decides whether it can speak that (the negotiation the spec describes).
//
// What each adds is optional for a server that offers only tools: 2025-03-26 brought tool
// annotations, which this endpoint serves, and the Streamable HTTP transport it already speaks;
// 2025-06-18 removed JSON-RPC batching, which it never accepted. 2024-11-05 stays because older
// desktop clients still ask for it.
var SupportedProtocolVersions = []string{ProtocolVersion, "2025-03-26", "2024-11-05"}

// NegotiateProtocolVersion answers a client's requested revision.
func NegotiateProtocolVersion(requested string) string {
	for _, v := range SupportedProtocolVersions {
		if v == requested {
			return v
		}
	}
	return ProtocolVersion
}

// ErrUnauthorized is the single answer to every "this credential is not usable"
// question: absent, malformed, expired, revoked, or bound to a disabled agent.
//
// One error for all of them on purpose. A caller that can tell "no such token" from
// "revoked token" from "agent disabled" can enumerate state it was never granted,
// and the distinction is worthless to a legitimate client — which either has a
// working credential or does not.
var ErrUnauthorized = errors.New("mcp: credential is not usable")
