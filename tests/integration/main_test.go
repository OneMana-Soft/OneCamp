//go:build integration
// +build integration

package integration

import (
	"fmt"
	"os"
	"testing"
)

// TestMain stops the servers the tests shared (shared.go) once every test has
// run, rather than leaving them to testcontainers' reaper, and fails the run
// if the tests left the environment other than they found it (teststate.go
// names the test; this catches what happened outside every Setup).
func TestMain(m *testing.M) {
	code := m.Run()
	stopServers()
	if d := environDiff(startEnviron, os.Environ()); d != "" {
		fmt.Printf("integration: the tests left the environment changed: %s\n", d)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
