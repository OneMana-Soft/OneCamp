//go:build integration
// +build integration

package integration

import (
	"fmt"
	"os"
	"testing"
)

// TestMain runs the tests through Main, as every integration package's does,
// and fails the run if the tests left the environment other than they found
// it (teststate.go names the test; this catches what happened outside every
// Setup).
func TestMain(m *testing.M) {
	code := Main(m)
	if d := environDiff(startEnviron, os.Environ()); d != "" {
		fmt.Printf("integration: the tests left the environment changed: %s\n", d)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
