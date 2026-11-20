//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"

	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	taskReadGeneration "github.com/akashc777/OneCamp/models/taskread/generation"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Every door (form, API, GitHub, mail) holds what arrives through
// triageBusiness.HoldAll, which keeps a batch in one PutAll statement. That
// statement moves the task-read generation once when any row went in, never
// once a row, and not at all when every task waits already, so the
// decision's kept reads (B326) stay right whichever door a task came by.
func TestHoldAllMovesTheGenerationOncePerStatement(t *testing.T) {
	d := newDecisionWorld(t)
	d.triageOn(t)
	ctx := context.Background()
	project := uuid.MustParse(d.project)
	generation := func() int64 {
		n, err := redisInit.RedisClient.Get(ctx, "taskread:gen").Int64()
		if err != nil && !errors.Is(err, redis.Nil) {
			t.Fatal(err)
		}
		return n
	}
	// The first move sets the shared generation's random floor; every move
	// after it adds one, so each step below counts its moves exactly.
	taskReadGeneration.Bump(ctx, "test: the floor")
	if generation() == 0 {
		t.Fatal("the generation has no floor to count from")
	}
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	for _, step := range []struct {
		name  string
		tasks []uuid.UUID
		moves int64
	}{
		{"three tasks arrive at once", []uuid.UUID{a, b, c}, 1},
		{"the same three again: nothing goes in", []uuid.UUID{a, b, c}, 0},
		{"one new beside two waiting", []uuid.UUID{a, b, uuid.New()}, 1},
	} {
		in := make([]triageBusiness.Arrival, len(step.tasks))
		for i, id := range step.tasks {
			in[i] = triageBusiness.Arrival{TaskID: id, From: triageBusiness.Requester{Name: "Visitor", Email: "visitor@example.com"}}
		}
		was := generation()
		if _, err := triageBusiness.HoldAll(ctx, project, triageBusiness.FromForm, in); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if moved := generation() - was; moved != step.moves {
			t.Fatalf("%s: HoldAll moved the task-read generation %d times, want %d (once a statement that put a row in)", step.name, moved, step.moves)
		}
	}
}
