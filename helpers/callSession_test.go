package helpers

import (
	"testing"
)

func TestIsCallSessionKey(t *testing.T) {
	cases := map[string]bool{
		"call-RM_abc123":   true,
		"  call-RM_abc  ":  true,
		"EG_xyz789":        false, // a real egress id
		"":                 false,
		"recall-something": false, // must be a prefix, not a substring
	}
	for key, want := range cases {
		if got := IsCallSessionKey(key); got != want {
			t.Errorf("IsCallSessionKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestCallSessionID(t *testing.T) {
	for key, want := range map[string]string{
		"call-RM_abc": "RM_abc",
		" call-RM_x ": "RM_x",
		"EG_xyz":      "",
		"":            "",
	} {
		if got := CallSessionID(key); got != want {
			t.Errorf("CallSessionID(%q) = %q, want %q", key, got, want)
		}
	}
	if CallSessionID(CallSessionKeyFor("RM_round")) != "RM_round" {
		t.Error("a session key does not give back its session")
	}
}
