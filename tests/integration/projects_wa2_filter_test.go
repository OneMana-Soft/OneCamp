//go:build integration
// +build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/models/taskread"
)

// The measurement's "filter off" (projects_wa2_scale_test.go): nothing
// decided, nothing left out, no read of Postgres for it.
func init() {
	wa2FilterOff = func(t *testing.T) func() {
		was := taskread.Decide
		// Put back when the test ends as well, should it stop before restoring.
		t.Cleanup(func() { taskread.Decide = was })
		taskread.Decide = func(context.Context, string) (map[string]bool, error) { return nil, nil }
		return func() { taskread.Decide = was }
	}
}
