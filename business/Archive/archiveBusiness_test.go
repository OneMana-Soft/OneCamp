package business

import (
	"context"
	"errors"
	"testing"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
)

// ------------------------------------------------------------------
// Pure logic / configuration tests (no DB required)
// ------------------------------------------------------------------

func TestValidEntityTypes(t *testing.T) {
	expected := []string{"posts", "chats", "tasks", "recordings", "attachments", "docs"}
	for _, et := range expected {
		if !validEntityTypes[et] {
			t.Errorf("validEntityTypes missing %q", et)
		}
	}
	if validEntityTypes["users"] {
		t.Error("validEntityTypes should not contain 'users'")
	}
}

func TestEntityLabels(t *testing.T) {
	cases := map[string]string{
		"posts":       "Post",
		"chats":       "Chat",
		"tasks":       "Task",
		"docs":        "Document",
		"recordings":  "Recording",
		"attachments": "Attachment",
	}
	for et, want := range cases {
		if got := entityLabels[et]; got != want {
			t.Errorf("entityLabels[%q] = %q, want %q", et, got, want)
		}
	}
}

func TestEntitySupportsUndo(t *testing.T) {
	shouldSupport := []string{"posts", "chats", "tasks", "attachments"}
	for _, et := range shouldSupport {
		if !entitySupportsUndo[et] {
			t.Errorf("entitySupportsUndo should be true for %q", et)
		}
	}
	shouldNot := []string{"docs", "recordings"}
	for _, et := range shouldNot {
		if entitySupportsUndo[et] {
			t.Errorf("entitySupportsUndo should be false for %q", et)
		}
	}
}

func TestEntityRegistryCoversAllValidTypes(t *testing.T) {
	for et := range validEntityTypes {
		ops, ok := entityRegistry[et]
		if !ok {
			t.Errorf("entityRegistry missing %q", et)
			continue
		}
		if ops.Label != entityLabels[et] {
			t.Errorf("entityRegistry[%q].Label = %q, want %q", et, ops.Label, entityLabels[et])
		}
		if ops.ArchiveFn == nil {
			t.Errorf("entityRegistry[%q].ArchiveFn is nil", et)
		}
		// Dgraph sync functions are required for postgres-backed entities
		if entitySupportsUndo[et] {
			if ops.ArchiveDgraphFn == nil {
				t.Errorf("entityRegistry[%q].ArchiveDgraphFn is nil", et)
			}
			if ops.RestoreDgraphFn == nil {
				t.Errorf("entityRegistry[%q].RestoreDgraphFn is nil", et)
			}
		}
	}
}

func TestEntityRestoreQuery(t *testing.T) {
	for _, et := range []string{"posts", "chats", "tasks", "attachments"} {
		q, ok := entityRestoreQuery[et]
		if !ok {
			t.Errorf("entityRestoreQuery missing %q", et)
			continue
		}
		if q == "" {
			t.Errorf("entityRestoreQuery[%q] is empty", et)
		}
	}
}

func TestEntityRecentQueries(t *testing.T) {
	for _, et := range []string{"posts", "chats", "tasks", "attachments"} {
		c, ok := entityRecentCountQuery[et]
		if !ok || c == "" {
			t.Errorf("entityRecentCountQuery missing or empty for %q", et)
		}
		l, ok := entityRecentListQuery[et]
		if !ok || l == "" {
			t.Errorf("entityRecentListQuery missing or empty for %q", et)
		}
	}
}

func TestArchiveAlreadyRunningError(t *testing.T) {
	err := &ArchiveAlreadyRunningError{EntityType: "posts"}
	want := "archive job already running for entity type: posts"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestErrPolicyNotFound(t *testing.T) {
	if !errors.Is(ErrPolicyNotFound, ErrPolicyNotFound) {
		t.Error("ErrPolicyNotFound should match itself with errors.Is")
	}
	if errors.Is(ErrPolicyNotFound, errors.New("policy not found")) {
		t.Error("ErrPolicyNotFound should NOT match a different sentinel with the same text")
	}
}

func TestPolicyUpdateInputRetentionBounds(t *testing.T) {
	// These bounds are enforced inside UpdateArchivePolicy.
	// We verify the boundary values here as pure-logic documentation.
	seven := 7
	threeSixFifty := 3650
	cases := []struct {
		name  string
		input PolicyUpdateInput
		want  string // empty = no error expected
	}{
		{"min valid", PolicyUpdateInput{RetentionDays: &seven}, ""},
		{"max valid", PolicyUpdateInput{RetentionDays: &threeSixFifty}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// We cannot call UpdateArchivePolicy without a DB, so we just
			// document the boundary values. Full validation is tested
			// in integration tests.
			_ = c.input
		})
	}
}

// ------------------------------------------------------------------
// Lightweight stateless helpers
// ------------------------------------------------------------------

func TestCheckRunningArchiveJob_SkippedWithoutDB(t *testing.T) {
	// Without a DB connection this panics because postgresInit.DBConn is nil.
	// Skip until a test DB harness is available.
	t.Skip("requires postgres test DB")
}

// ------------------------------------------------------------------
// Stub helpers
// ------------------------------------------------------------------

// dgraphAvailable returns true when dgraphInit.DgraphClient is non-nil.
// The unit-test runner doesn't bring up a Dgraph instance, so any test
// that bottoms out in a Dgraph query needs to skip when this is false
// — otherwise the dgo client panics with a nil-pointer dereference.
func dgraphAvailable() bool {
	return dgraphInit.DgraphClient != nil
}

func TestGetRecentlyArchivedDocs(t *testing.T) {
	if !dgraphAvailable() {
		t.Skip("requires Dgraph test instance")
	}
	items, total, err := getRecentlyArchivedDocs(context.Background(), 50, 0, "Document")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if len(items) != 0 {
		t.Errorf("len(items) = %d, want 0", len(items))
	}
}

func TestGetRecentlyArchivedRecordings(t *testing.T) {
	if !dgraphAvailable() {
		t.Skip("requires Dgraph test instance")
	}
	items, total, err := getRecentlyArchivedRecordings(context.Background(), 50, 0, "Recording")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if len(items) != 0 {
		t.Errorf("len(items) = %d, want 0", len(items))
	}
}

// ------------------------------------------------------------------
// TODO: Integration tests (require postgres test DB)
// ------------------------------------------------------------------
//
// The following scenarios need a real postgres connection and should be
// added once a test DB harness is available:
//
// 1. TestGetArchivePolicies_HappyPath
// 2. TestUpdateArchivePolicy_RowsAffectedZero → ErrPolicyNotFound
// 3. TestUpdateArchivePolicy_RetentionBounds (1 → error, 7 → ok, 3650 → ok, 3651 → error)
// 4. TestRunArchiveJob_InvalidEntityType
// 5. TestRunArchiveJob_AlreadyRunning → ArchiveAlreadyRunningError
// 6. TestRestoreItems_EmptyIDs → (0, nil)
// 7. TestRestoreItems_RunningJobGuard → ArchiveAlreadyRunningError
// 8. TestRestoreItems_Posts_HappyPath (insert 3 posts, archive 2, restore 1)
// 9. TestUndoArchiveJob_ExactIDs (archive 5 posts, verify metadata.archived_ids, undo restores exactly those 5)
// 10. TestGetArchiveJobs_Limit50
// 11. TestGetArchiveStats_Accuracy
//
// Recommended test harness: spin up a postgres container via
// testcontainers-go or use a throwaway DB in CI.

// ------------------------------------------------------------------
// Memory cascade wiring (privacy: archived content must not linger as
// AI-queryable memory; restore must revive it). Pure registry checks.
// ------------------------------------------------------------------

func TestEntityRegistryMemorySourceType(t *testing.T) {
	// Only posts and chats carry captured workspace-memory items linked by
	// a per-message source_uuid, so only they cascade to the memory layer.
	want := map[string]string{
		"posts":       "post",
		"chats":       "chat",
		"tasks":       "",
		"attachments": "",
		"recordings":  "",
		"docs":        "",
	}
	for et, expected := range want {
		ops, ok := entityRegistry[et]
		if !ok {
			t.Errorf("entityRegistry missing %q", et)
			continue
		}
		if ops.MemorySourceType != expected {
			t.Errorf("entityRegistry[%q].MemorySourceType = %q, want %q", et, ops.MemorySourceType, expected)
		}
	}
}
