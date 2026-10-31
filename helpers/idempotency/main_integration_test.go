//go:build integration

package idempotency

import (
	"os"
	"testing"

	"github.com/akashc777/OneCamp/tests/integration"
)

func TestMain(m *testing.M) { os.Exit(integration.Main(m)) }
