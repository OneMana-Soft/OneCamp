//go:build integration
// +build integration

package integration_test

// An installed app's token can't call /v1/mcp (A10): MCP's tools aren't held
// to the places an app was added to. AI edition only, as MCP is.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnAppsTokenCantUseMCP -v

import (
	"net/http"
	"testing"

	mcpBusiness "github.com/akashc777/OneCamp/business/MCPServer"
)

func TestAnAppsTokenCantUseMCP(t *testing.T) {
	fx := seedAppInOneChannel(t)
	code, out := fx.call("POST", "/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	if c, words := refusal(out); code != http.StatusForbidden || c != "not_for_apps" || words != mcpBusiness.AppsCantUseMCP {
		t.Errorf("an app's token on /v1/mcp: %d %v, want 403 not_for_apps %q", code, out, mcpBusiness.AppsCantUseMCP)
	}
}
