package business

import (
	"context"
	"strings"
	"testing"
)

// TestValidateAgentModelPref_EmptyIsDefault verifies that an unset/blank model
// preference is accepted (the agent runs on the workspace default) without any
// allowlist lookup.
func TestValidateAgentModelPref_EmptyIsDefault(t *testing.T) {
	ctx := context.Background()
	blank := "   "
	for _, in := range []*string{nil, ptr(""), &blank} {
		if err := validateAgentModelPref(ctx, in); err != nil {
			t.Fatalf("expected nil error for empty model pref, got %v", err)
		}
	}
}

// TestValidateAgentModelPref_RejectsMalformedID verifies a non-UUID model
// preference is rejected up front (before any DB lookup), so a bad client value
// can't reach the allowlist query.
func TestValidateAgentModelPref_RejectsMalformedID(t *testing.T) {
	ctx := context.Background()
	bad := "not-a-uuid"
	err := validateAgentModelPref(ctx, &bad)
	if err == nil {
		t.Fatal("expected error for malformed model id")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "invalid model selection") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func ptr(s string) *string { return &s }
