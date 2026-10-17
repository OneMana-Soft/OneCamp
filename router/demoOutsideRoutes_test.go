package router

import (
	"os"
	"strings"
	"testing"
)

// The demo's shared visitor can't add, change, switch, test or remove an MCP
// server, or try a remote agent's address: each such route carries the
// demo guard. Reading the list stays open.
func TestTheDemoVisitorCannotChangeWhatAgentsReachOutside(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, route := range []string{
		`.Post("/mcp/servers", mcpController.CreateServer)`,
		`.Post("/mcp/servers/{id}/update", mcpController.UpdateServer)`,
		`.Post("/mcp/servers/{id}/enabled", mcpController.SetEnabled)`,
		`.Post("/mcp/servers/{id}/delete", mcpController.DeleteServer)`,
		`.Post("/mcp/servers/{id}/test", mcpController.TestConnection)`,
		`.Post("/agents/agui/check", agentController.CheckRemoteBrain)`,
	} {
		if n := strings.Count(body, route); n != 1 {
			t.Errorf("%s is routed %d times", route, n)
			continue
		}
		if !strings.Contains(body, "r.With(customMiddleware.NoOutsideConnectionsInDemo)"+route) {
			t.Errorf("%s is not behind NoOutsideConnectionsInDemo", route)
		}
	}
}
