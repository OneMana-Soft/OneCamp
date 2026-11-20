//go:build integration

package notification

import (
	"os"
	"testing"

	"github.com/akashc777/OneCamp/tests/integration"
)

// TestMain stops the harness's servers when this package's tests end.
func TestMain(m *testing.M) { os.Exit(integration.Main(m)) }
