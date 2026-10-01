package business

// Hand-offs, said in the open.
//
// WHY. An agent can hand work to another agent by mentioning it, and the chain
// that makes that safe (hop budget, cycle check, a person at the root) has been
// stored on every task since migration 137. None of it reached the reader: the
// second agent's reply looked like it had been asked directly, so nobody could
// tell who passed the work along or whom it was ultimately for. The reply now
// opens with one plain line that says both, on every surface a message appears
// on, with nothing new to store or render.

import (
	"context"
	"strings"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// previousAgentInChain is the agent that handed the work to self: the last id
// in the chain that is not self. The durable path stores the chain before this
// agent's turn and the synchronous path after it, so both are read the same
// way. Empty when nobody handed it over. Pure.
func previousAgentInChain(chain []string, self string) string {
	self = strings.TrimSpace(self)
	for i := len(chain) - 1; i >= 0; i-- {
		if id := strings.TrimSpace(chain[i]); id != "" && id != self {
			return id
		}
	}
	return ""
}

// handoffLine is the sentence itself. Empty without a handing agent. Pure.
func handoffLine(fromAgent, forPerson string) string {
	fromAgent, forPerson = strings.TrimSpace(fromAgent), strings.TrimSpace(forPerson)
	if fromAgent == "" {
		return ""
	}
	// A name can end in a full stop ("Priya N."); the sentence adds its own.
	end := func(s string) string { return strings.TrimRight(s, ".") + "." }
	if forPerson == "" {
		return end("Picked up from " + fromAgent)
	}
	return end("Picked up from " + fromAgent + ", for " + forPerson)
}

// seams for tests
var (
	agentNameFn = func(ctx context.Context, id uuid.UUID) string {
		a, err := model.GetAgentByID(ctx, id)
		if err != nil || a == nil {
			return ""
		}
		return strings.TrimSpace(a.Name)
	}
	personNameFn = steeringAuthorName
)

// withHandoff puts the hand-off line above a reply when the work came from
// another agent, and returns the reply unchanged otherwise.
func withHandoff(ctx context.Context, self uuid.UUID, chain []string, origin, body string) string {
	prev := previousAgentInChain(chain, self.String())
	if prev == "" {
		return body
	}
	prevID, err := uuid.Parse(prev)
	if err != nil {
		return body
	}
	person := ""
	if o, perr := uuid.Parse(strings.TrimSpace(origin)); perr == nil {
		person = personNameFn(ctx, o)
	}
	line := handoffLine(agentNameFn(ctx, prevID), person)
	if line == "" {
		return body
	}
	return line + "\n\n" + body
}
