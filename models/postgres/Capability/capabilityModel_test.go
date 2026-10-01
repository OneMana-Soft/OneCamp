package models

import "testing"

func TestValidCapability_OnlyWiredCapabilitiesAccepted(t *testing.T) {
	// Wired, member-delegatable capabilities are accepted.
	if !ValidCapability(CapWorkflowManage) {
		t.Error("workflow.manage must be a valid (wired) capability")
	}
	if !ValidCapability(CapInvitationCreate) {
		t.Error("invitation.create must be a valid (wired) capability")
	}
	// Security-sensitive capabilities are intentionally NOT in the catalog, so
	// the policy engine rejects attempts to set them — no dead toggle.
	if ValidCapability(CapAppManage) {
		t.Error("app.manage must NOT be delegatable yet (admin-only)")
	}
	if ValidCapability(CapWebhookManage) {
		t.Error("webhook.manage must NOT be delegatable yet (admin-only)")
	}
	if ValidCapability("totally.unknown") {
		t.Error("unknown capability must be rejected")
	}
}

func TestValidPolicy(t *testing.T) {
	if !ValidPolicy(PolicyAdminsOnly) || !ValidPolicy(PolicyAllMembers) {
		t.Error("the two known policies must be valid")
	}
	if ValidPolicy("everyone") || ValidPolicy("") {
		t.Error("unknown policy values must be rejected")
	}
}
