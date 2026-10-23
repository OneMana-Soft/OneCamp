package controllers

import (
	"net/http"

	mcpBusiness "github.com/akashc777/OneCamp/business/MCPServer"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
)

// OneCamp as an MCP server on the public API (AI edition): one JSON-RPC
// endpoint for any MCP-capable client. The token is the API's own; each tool
// checks its scope inside HandleRPC, so a token only sees and calls what its
// scopes allow, and the tools keep their own read and write limits (MCPToolRead
// and MCPToolWriteRate) under this route's. An app's token is refused here and
// again by ResolveActor, in the same words.
func init() {
	v1api.Register(v1api.Resource{
		Name: "mcp", Owner: "platform",
		Routes: []v1api.Route{
			{Method: http.MethodPost, Path: "/mcp", Summary: "The MCP server (JSON-RPC): workspace tools for AI clients", ScopeInside: true,
				AppsRefused: mcpBusiness.AppsCantUseMCP, Limit: v1api.Limit{PerMinute: 1200}, Legacy: true, Handler: HandleRPC},
		},
	})
}
