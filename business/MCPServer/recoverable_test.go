package business

import (
	"encoding/json"
	"strings"
	"testing"

	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
)

// A destructive tool the server itself takes back runs with no person's
// approval, as an additive write does; one it can't take back still asks.
// A Recoverable op whose undo isn't published beside it isn't published at
// all, so the server doesn't start (cmd/server: PublishOps).
func TestARecoverableToolNeedsNoApprovalOnlyWithItsUndoBesideIt(t *testing.T) {
	if RequiresApproval(writeSpec("t", ToolBehaviour{Destructive: true, Recoverable: true})) {
		t.Error("a destructive write the server takes back asks a person to approve it")
	}
	if !RequiresApproval(writeSpec("t", ToolBehaviour{Destructive: true})) {
		t.Error("a destructive write the server can't take back runs unapproved")
	}
	drop := v1api.Op{Name: "things.drop", Summary: "Drop a thing", Scope: "things:write",
		Behaviour: v1api.Behaviour{Destructive: true, Recoverable: true}, UndoneBy: []string{"things.restore"}}
	if err := publish([]v1api.Op{drop}); err == nil || !strings.Contains(err.Error(), "things.restore") {
		t.Errorf("publishing a Recoverable op without its restore answered %v; want it refused, naming things.restore", err)
	}
}

// A refusal's details reach an MCP caller in its structured error, and in its
// text, one line each, for clients that hand their model only the text.
func TestARefusalsDetailsReachTheCallerInItsText(t *testing.T) {
	rows := &ToolRefusal{Code: "invalid_argument", Message: "2 rows can't be written",
		Details: []map[string]string{{"row": "r1", "message": "it changed"}, {"row": "r2", "message": "no such row"}}}
	if got, want := rows.Text(), "2 rows can't be written\n"+`{"message":"it changed","row":"r1"}`+"\n"+`{"message":"no such row","row":"r2"}`; got != want {
		t.Errorf("Text() = %q, want %q", got, want)
	}
	if got := (&ToolRefusal{Message: "one", Details: map[string]int{"limit": 3}}).Text(); got != "one\n"+`{"limit":3}` {
		t.Errorf("a refusal with one detail reads %q", got)
	}
	if got := (&ToolRefusal{Message: "plain"}).Text(); got != "plain" {
		t.Errorf("a refusal with no details reads %q, want its message alone", got)
	}
	b, _ := json.Marshal(rows)
	if !strings.Contains(string(b), `"details":[{"message":"it changed","row":"r1"}`) {
		t.Errorf("the structured refusal %s carries no details", b)
	}
	if b, _ := json.Marshal(&ToolRefusal{Code: "x", Message: "y"}); strings.Contains(string(b), "details") {
		t.Errorf("a refusal with no details still names them: %s", b)
	}
}
