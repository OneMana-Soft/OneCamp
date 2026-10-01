//go:build integration

package liveness

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// Every check runs against the real schema. A table or column renamed under
// one of them would make its check fail, and a failed check keeps its results,
// so the filter would quietly stop working; this is what notices.
func TestEveryCheckRunsAgainstTheRealSchema(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupDgraph(t)
	missing := uuid.NewString()
	for contentType, check := range Checkers {
		got, err := check(ctx, []string{missing})
		if err != nil {
			t.Errorf("%s: %v", contentType, err)
			continue
		}
		if _, found := got[missing]; found {
			t.Errorf("%s: content that was never created reads as found", contentType)
		}
	}
}
