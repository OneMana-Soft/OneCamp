package controllers

import (
	"net/http"

	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	"github.com/akashc777/OneCamp/helpers"
)

// OneCamp as an MCP server is the AI edition's (its controllers/MCP registers
// /v1/mcp). This edition has no AI, so the route answers that, as it did before
// the public API's registry, and both editions describe the same /v1 routes but
// /attention.
func init() {
	v1api.Register(v1api.Resource{
		Name: "mcp", Owner: "platform",
		Routes: []v1api.Route{
			{Method: http.MethodPost, Path: "/mcp", Summary: "The MCP server: in the AI edition only", ScopeInside: true,
				Limit: v1api.Limit{PerMinute: 1200}, Legacy: true, Handler: mcpNotInThisEdition},
		},
	})
}

func mcpNotInThisEdition(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
}
