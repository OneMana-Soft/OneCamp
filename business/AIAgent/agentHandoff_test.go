package business

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestTheAgentThatHandedWorkOverIsTheLastOtherInTheChain(t *testing.T) {
	a, b, self := uuid.NewString(), uuid.NewString(), uuid.NewString()
	cases := []struct {
		chain []string
		want  string
	}{
		{nil, ""},                 // asked directly
		{[]string{self}, ""},      // synchronous path, first hop
		{[]string{a}, a},          // durable path: chain before this turn
		{[]string{a, self}, a},    // synchronous path: chain after it
		{[]string{a, b, self}, b}, // the nearest one handed it over
	}
	for _, c := range cases {
		if got := previousAgentInChain(c.chain, self); got != c.want {
			t.Errorf("previousAgentInChain(%v) = %q, want %q", c.chain, got, c.want)
		}
	}
}

func TestTheReplySaysWhoHandedItOverAndForWhom(t *testing.T) {
	oldA, oldP := agentNameFn, personNameFn
	t.Cleanup(func() { agentNameFn, personNameFn = oldA, oldP })
	agentNameFn = func(context.Context, uuid.UUID) string { return "Release Captain" }
	personNameFn = func(context.Context, uuid.UUID) string { return "Priya N." }
	self, prev, origin := uuid.New(), uuid.NewString(), uuid.NewString()

	got := withHandoff(context.Background(), self, []string{prev}, origin, "Notes are drafted.")
	if got != "Picked up from Release Captain, for Priya N.\n\nNotes are drafted." {
		t.Fatalf("got %q", got)
	}
	if got := withHandoff(context.Background(), self, nil, origin, "Hi."); got != "Hi." {
		t.Fatalf("a direct request gained a hand-off line: %q", got)
	}
	personNameFn = func(context.Context, uuid.UUID) string { return "" }
	if got := withHandoff(context.Background(), self, []string{prev}, "", "Ok."); got != "Picked up from Release Captain.\n\nOk." {
		t.Fatalf("got %q", got)
	}
	agentNameFn = func(context.Context, uuid.UUID) string { return "" }
	if got := withHandoff(context.Background(), self, []string{prev}, origin, "Ok."); got != "Ok." {
		t.Fatalf("an unnamed agent still produced a line: %q", got)
	}
}
