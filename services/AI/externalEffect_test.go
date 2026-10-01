package ai

import (
	"strings"
	"testing"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
)

// The three tools whose effect leaves the workspace and cannot be recalled.
//
// Written out rather than derived so that adding a fourth is a deliberate edit to
// a list a reviewer can read, and so that REMOVING the flag from one of these
// fails instead of silently shrinking the set the backstop protects.
var wantExternalEffect = map[string]string{
	"gmail_send":            "delivers mail to a third party; nothing in OneCamp unsends it",
	"calendar_create_event": "notifies attendees; deleting the event does not recall the invitations",
	"github_comment":        "publishes under the connected human's name to everyone watching the thread",
}

// TestExternalEffectSetIsExactlyTheIrrecoverableWrites pins the flag to the set
// above in BOTH directions.
//
// The reverse direction is the one that matters in practice: flagging an ordinary
// tool would route it through human approval, and for send_message — how an agent
// says anything — that would turn every reply into an approval card and make
// autonomous agents useless. So over-flagging is as much a defect as under-flagging
// and this test refuses both.
func TestExternalEffectSetIsExactlyTheIrrecoverableWrites(t *testing.T) {
	got := map[string]bool{}
	for _, tool := range ToolRegistry {
		if tool.ExternalEffect {
			got[tool.Name] = true
			if tool.ReadOnly {
				t.Errorf("%s: ReadOnly and ExternalEffect are contradictory — a read has no effect to leave the workspace", tool.Name)
			}
		}
	}
	for name, why := range wantExternalEffect {
		if !got[name] {
			t.Errorf("%s must be ExternalEffect: %s", name, why)
		}
		delete(got, name)
	}
	for name := range got {
		t.Errorf("%s is flagged ExternalEffect but is not in the reviewed list. If its effect really does "+
			"leave the workspace irrecoverably, add it to wantExternalEffect with the reason. If it does "+
			"not, remove the flag: flagging an internal tool sends it to human approval, and doing that to "+
			"send_message would make every agent reply wait for a click.", name)
	}
}

// TestDocumentedConfirmationIsEnforced binds the prose to the code.
//
// Three tool descriptions end "Requires confirmation." That sentence is fed to a
// model as part of a prompt, and a prompt cannot enforce anything — the model is
// free to ignore it, and in full autonomy nothing downstream was checking. This
// test asserts that any tool making that promise in its description is actually
// held to it by the gate predicate, so the two can never drift apart again: a new
// tool that documents confirmation and forgets the flag fails here.
func TestDocumentedConfirmationIsEnforced(t *testing.T) {
	found := 0
	for _, tool := range ToolRegistry {
		if !strings.Contains(strings.ToLower(tool.Description), "requires confirmation") {
			continue
		}
		found++
		if !ToolNeedsHumanBeforeUnattended(tool.Name) {
			t.Errorf("%s documents \"Requires confirmation\" but ToolNeedsHumanBeforeUnattended is false, "+
				"so an agent in full autonomy runs it with no confirmation at all. Either set "+
				"ExternalEffect on it or stop promising confirmation in its description.", tool.Name)
		}
	}
	if found == 0 {
		t.Fatal("no tool description mentions confirmation — this test silently stopped checking anything")
	}
}

// TestExternalEffectToolsAreNotOnThePublicSurface encodes the reason ToolScope
// already gives for leaving them out: they need interactive OAuth and a human
// confirmation, neither of which a bearer token on an MCP or REST call can supply.
//
// Vacuously true today, which is the point — it is the edit that would break it
// that this guards. Adding gmail_send to ToolScope would expose "send mail as this
// person" to anything holding a token, and the destructive-hint plumbing that would
// let a client know to ask first does not exist yet.
func TestExternalEffectToolsAreNotOnThePublicSurface(t *testing.T) {
	for _, tool := range ToolRegistry {
		if !tool.ExternalEffect {
			continue
		}
		if scope, public := apiTokenBusiness.ScopeForTool(tool.Name); public {
			t.Errorf("%s is ExternalEffect but exposed publicly under scope %q. A token cannot supply the "+
				"interactive confirmation this tool's own description requires. Remove it from ToolScope, "+
				"or first give the MCP surface a way to demand approval before the call runs.", tool.Name, scope)
		}
	}
}

// TestNeedsHumanPredicateCombinesBothFacts covers the predicate's own logic rather
// than the data, including the dynamic (remote MCP) source that ExternalEffect
// deliberately does not replace.
func TestNeedsHumanPredicateCombinesBothFacts(t *testing.T) {
	// A registry tool with neither fact runs unattended.
	if ToolNeedsHumanBeforeUnattended("list_tasks") {
		t.Error("list_tasks is an ordinary read and must not require approval")
	}
	if ToolNeedsHumanBeforeUnattended("update_task_status") {
		t.Error("update_task_status is a reversible internal write and must not require approval")
	}
	// ExternalEffect alone is enough, without Destructive.
	if ToolIsDestructive("gmail_send") {
		t.Error("gmail_send must NOT be Destructive: under MCP's destructiveHint a new email is an " +
			"additive change, and claiming otherwise would advertise a value the spec calls wrong")
	}
	if !ToolNeedsHumanBeforeUnattended("gmail_send") {
		t.Error("gmail_send must require a human via ExternalEffect even though it is not Destructive")
	}
	// An unknown name reaches no executor, so there is nothing to gate.
	if ToolNeedsHumanBeforeUnattended("no_such_tool_xyz") {
		t.Error("unknown tools must not be reported as needing approval")
	}
}
