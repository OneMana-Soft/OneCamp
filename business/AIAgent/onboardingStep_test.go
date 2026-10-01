package business

import (
	"context"
	"errors"
	"testing"

	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestAgentStepDone(t *testing.T) {
	old := anyAgentExists
	t.Cleanup(func() { anyAgentExists = old })
	for _, c := range []struct {
		exists bool
		err    error
		want   bool
	}{{true, nil, true}, {false, nil, false}, {true, errors.New("db"), false}} {
		anyAgentExists = func(context.Context) (bool, error) { return c.exists, c.err }
		if got := agentStepDone(context.Background(), userModels.UserInfo{}); got != c.want {
			t.Errorf("exists=%v err=%v: got %v", c.exists, c.err, got)
		}
	}
}
