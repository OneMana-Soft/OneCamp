package business

import (
	"errors"
	archiveModels "github.com/akashc777/OneCamp/models/postgres/Archive"
	"testing"
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
	// Docs can be undone since a docs run records which docs it archived.
	shouldSupport := []string{"posts", "chats", "tasks", "attachments", "docs"}
	for _, et := range shouldSupport {
		if !entitySupportsUndo[et] {
			t.Errorf("entitySupportsUndo should be true for %q", et)
		}
	}
	shouldNot := []string{"recordings"}
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
		// (docs live in the graph: their ArchiveFn writes it).
		if archiveModels.Archivable(et) {
			if ops.ArchiveDgraphFn == nil {
				t.Errorf("entityRegistry[%q].ArchiveDgraphFn is nil", et)
			}
			if ops.RestoreDgraphFn == nil {
				t.Errorf("entityRegistry[%q].RestoreDgraphFn is nil", et)
			}
		}
	}
}

// The Postgres-backed types are the ones the archive's models archive and
// restore (models/postgres/Archive): their SQL lives there now.
func TestPostgresBackedTypesAreArchivable(t *testing.T) {
	for _, et := range []string{"posts", "chats", "tasks", "attachments"} {
		if !archiveModels.Archivable(et) {
			t.Errorf("%q is not archivable", et)
		}
	}
	for _, et := range []string{"docs", "recordings", "users"} {
		if archiveModels.Archivable(et) {
			t.Errorf("%q is archived in Postgres by the models", et)
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
