package business

import (
	"testing"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// The backstop's default answer for each class of tool. This is the condition that
// decides, in full autonomy with nobody watching, whether an action happens or waits
// for a person.
func TestUnattendedApprovalRequiredByDefault(t *testing.T) {
	t.Setenv("AI_ALLOW_AGENT_DESTRUCTIVE_AUTORUN", "")

	for _, tool := range []string{"gmail_send", "calendar_create_event", "github_comment"} {
		if !unattendedApprovalRequired(tool) {
			t.Errorf("%s must require approval unattended: its effect leaves the workspace and no "+
				"autonomy setting should let an agent do it while nobody is watching", tool)
		}
	}
	// Reversible internal work is the agent's job and must not be gated, or an
	// autonomous agent cannot do anything without a person clicking.
	for _, tool := range []string{"list_tasks", "create_task", "update_task_status", "send_message", "update_table_row"} {
		if unattendedApprovalRequired(tool) {
			t.Errorf("%s is ordinary reversible work and must run unattended", tool)
		}
	}
	// code_pr is gated too. It used to be left out on the reasoning that the review
	// before merge is its human gate, but that review decides whether the change
	// lands, not whether a branch is pushed and a pull request opened under a
	// person's GitHub identity — which notifies everyone watching the repository and
	// is not taken back by closing it. The approval is the person whose account it
	// pushes with saying yes to that (see codePRProposal).
	if !unattendedApprovalRequired("code_pr") {
		t.Error("code_pr must require approval unattended: it pushes as a person, and that person " +
			"has to agree before it does")
	}
}

// The opt-out is a documented escape hatch for deployments that accept the risk.
// Tested because it had no coverage and inverting it silently disables the backstop
// for every tool at once — the failure would be invisible until something
// irreversible had already happened.
func TestDestructiveAutorunOptOut(t *testing.T) {
	for _, on := range []string{"1", "true", "TRUE", "yes", "on", " true "} {
		t.Setenv("AI_ALLOW_AGENT_DESTRUCTIVE_AUTORUN", on)
		if !destructiveAutorunAllowed() {
			t.Errorf("%q should enable autorun", on)
		}
		if unattendedApprovalRequired("gmail_send") {
			t.Errorf("%q: opt-out must lift the backstop", on)
		}
	}
	// Anything else must leave the backstop ON. A typo in a deployment's env file
	// must fail safe, not quietly permit unattended irreversible actions.
	for _, off := range []string{"", "0", "false", "no", "off", "ture", "maybe", "2"} {
		t.Setenv("AI_ALLOW_AGENT_DESTRUCTIVE_AUTORUN", off)
		if destructiveAutorunAllowed() {
			t.Errorf("%q must NOT enable autorun — an unrecognised value has to fail safe", off)
		}
		if !unattendedApprovalRequired("gmail_send") {
			t.Errorf("%q: backstop must stay on", off)
		}
	}
}

// Guards the reason the predicate exists at all: it must reflect BOTH facts, so a
// remote MCP server's destructiveHint and one of our own external-effect tools are
// treated the same way by the gate. If someone narrows it back to ToolIsDestructive,
// the second group silently loses its approval requirement.
func TestBackstopCoversOurOwnToolsNotJustRemoteOnes(t *testing.T) {
	t.Setenv("AI_ALLOW_AGENT_DESTRUCTIVE_AUTORUN", "")
	if ai.ToolIsDestructive("gmail_send") {
		t.Fatal("precondition: gmail_send is not Destructive (an email is additive under MCP's hint), " +
			"which is exactly why the backstop cannot rely on that flag alone")
	}
	if !unattendedApprovalRequired("gmail_send") {
		t.Error("the backstop must catch our own irreversible tools even though they carry no destructiveHint")
	}
}
