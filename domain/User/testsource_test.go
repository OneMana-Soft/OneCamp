package domain

import (
	"os"
	"testing"
)

func readUserDomainSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("userDomian.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	return string(b)
}
