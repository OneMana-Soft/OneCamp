package domain

import "testing"

func TestDgraphUIDOrNone(t *testing.T) {
	if got := dgraphUIDOrNone("0x1234"); got != "0x1234" {
		t.Fatalf("expected uid to be passed through, got %q", got)
	}
	if got := dgraphUIDOrNone(""); got != "0x1" {
		t.Fatalf("expected empty uid to become placeholder, got %q", got)
	}
	if got := dgraphUIDOrNone("   "); got != "0x1" {
		t.Fatalf("expected whitespace uid to become placeholder, got %q", got)
	}
}
