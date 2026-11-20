package domain

import (
	"strings"
	"testing"
)

// Removing a mailbox wipes its sealed secret in the same statement that
// soft-deletes it, and the due scan never claims a removed, paused or leased
// mailbox (MAIN condition 5).
func TestRemoveWipesSecretAndStopsPolling(t *testing.T) {
	for _, want := range []string{"secret_sealed = ''", "deleted_at = NOW()", "deleted_by = $2", "deleted_at IS NULL"} {
		if !strings.Contains(RemoveMailbox, want) {
			t.Errorf("RemoveMailbox lacks %q", want)
		}
	}
	for _, want := range []string{"deleted_at IS NULL", "paused_reason IS NULL", "leased_until", "FOR UPDATE SKIP LOCKED", "LIMIT $2"} {
		if !strings.Contains(ClaimDue, want) {
			t.Errorf("ClaimDue lacks %q", want)
		}
	}
	if !strings.Contains(ClaimKeys, "ON CONFLICT (project_uuid, message_key) DO UPDATE") || !strings.Contains(ClaimKeys, "task_uuid IS NULL") {
		t.Errorf("ClaimKeys is not a unique-key claim: %s", ClaimKeys)
	}
}
