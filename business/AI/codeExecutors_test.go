package business

import (
	"strings"
	"testing"
)

// topLevelEntries should collapse a flat blob path list to deduped, sorted
// top-level entries (root files as-is, nested paths as "dir/"), capped.
func TestTopLevelEntries(t *testing.T) {
	paths := []string{
		"README.md",
		"cmd/server/main.go",
		"cmd/worker/main.go",
		"business/AI/x.go",
		"README.md", // dup
		"/go.mod",
	}
	got := topLevelEntries(paths, 25)
	joined := strings.Join(got, ",")
	for _, want := range []string{"README.md", "cmd/", "business/", "go.mod"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %q in %v", want, got)
		}
	}
	// "cmd/" must appear once despite two nested files.
	if c := strings.Count(joined, "cmd/"); c != 1 {
		t.Fatalf("expected cmd/ once, got %d in %v", c, got)
	}
	// Sorted ascending.
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("not sorted: %v", got)
		}
	}
}

func TestTopLevelEntriesCap(t *testing.T) {
	var paths []string
	for i := 0; i < 50; i++ {
		paths = append(paths, string(rune('a'+i%26))+"x/file.go")
	}
	if got := topLevelEntries(paths, 5); len(got) > 5 {
		t.Fatalf("cap not honored: %d", len(got))
	}
}

func TestIsGitHubUnavailable(t *testing.T) {
	if !isGitHubUnavailable(errString("401 Unauthorized")) {
		t.Fatal("expected 401 treated as unavailable")
	}
	if isGitHubUnavailable(nil) {
		t.Fatal("nil should not be unavailable")
	}
	if isGitHubUnavailable(errString("some parse error")) {
		t.Fatal("unrelated error should not be unavailable")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
