package business

import (
	"os"
	"testing"
)

// readSource returns a file from this package, for the invariants that live in
// the shape of the code rather than in a value a test can call.
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
