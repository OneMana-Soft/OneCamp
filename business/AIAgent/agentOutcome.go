// Agent outcomes: what people actually did with what an agent proposed.
//
// WHY THIS IS NOT A THUMBS WIDGET. The eval pass rate already answers "does
// this agent do what I told it to on my test cases". It cannot answer "is this
// agent worth having", because a scenario suite is written by the same person
// who wrote the agent and scores it against their own expectations. The
// question that matters to a user is whether the work it produced was wanted,
// and the honest answer to that is a decision somebody already makes: approving
// or denying a proposed write.
//
// That signal is better than a rating for the reasons ratings are weak. A
// thumbs is an extra step nobody is paid to take, so it is sparse and skews
// negative, because satisfied people do not click. An approval is a decision on
// the critical path of real work: it is dense, it is not optional, and it is
// about a specific thing the agent wanted to do rather than a general feeling
// about the agent.
//
// The one thing it cannot see is an agent whose writes all run unattended
// (AutonomyFull proposes nothing), which is why this is reported next to the
// pass rate rather than instead of it.
package business

import (
	"context"

	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	"github.com/google/uuid"
)

// OutcomeUnmeasured is the acceptance rate of an agent nobody has decided about
// yet.
//
// A sentinel rather than 0, matching passRate, because zero is a real and very
// different answer: "every proposal was rejected" must not render the same as
// "there is nothing to report". Callers test for it explicitly.
const OutcomeUnmeasured = -1.0

// AgentOutcome is one agent's acceptance record, ready to render.
type AgentOutcome struct {
	Approved int `json:"approved"`
	Rejected int `json:"rejected"`
	Expired  int `json:"expired"`
	Pending  int `json:"pending"`
	Failed   int `json:"failed"`

	// Decided is Approved + Rejected: the proposals a person actually ruled on.
	// It is the denominator, and it is reported so a caller can say "2 of 3"
	// rather than only a percentage, which reads as far more evidence than two
	// decisions deserve.
	Decided int `json:"decided"`

	// AcceptanceRate is Approved/Decided, or OutcomeUnmeasured when nobody has
	// decided yet.
	AcceptanceRate float64 `json:"acceptance_rate"`

	// Ignored reports that proposals are expiring rather than being answered.
	// Deliberately separate from a low acceptance rate: an agent people argue
	// with is engaged with, an agent people scroll past is not, and the second
	// is usually the one to turn off.
	Ignored bool `json:"ignored"`
}

// ignoredThreshold is the share of finished proposals that must have expired
// unanswered before an agent is called ignored.
//
// Half, because below that the expiries read as ordinary noise (someone was on
// holiday, a proposal arrived at the end of a day). Above it, the agent is
// talking to nobody.
const ignoredThreshold = 0.5

// minIgnoredSample is how many finished proposals are needed before Ignored is
// worth showing. One expiry out of one proposal is 100% and means nothing.
const minIgnoredSample = 3

// summarize turns raw status counts into the rendered shape. Split out so the
// single-agent and batch paths cannot disagree about what a rate means, the
// same reason markStale exists in the eval code.
func summarize(c *pendingModels.AgentOutcomeCounts) *AgentOutcome {
	if c == nil {
		return &AgentOutcome{AcceptanceRate: OutcomeUnmeasured}
	}
	o := &AgentOutcome{
		Approved:       c.Approved,
		Rejected:       c.Rejected,
		Expired:        c.Expired,
		Pending:        c.Pending,
		Failed:         c.Failed,
		Decided:        c.Approved + c.Rejected,
		AcceptanceRate: OutcomeUnmeasured,
	}
	if o.Decided > 0 {
		o.AcceptanceRate = float64(o.Approved) / float64(o.Decided)
	}
	if finished := o.Decided + o.Expired; finished >= minIgnoredSample {
		o.Ignored = float64(o.Expired)/float64(finished) >= ignoredThreshold
	}
	return o
}

// AgentOutcomeBatch returns the acceptance record for every agent the actor may
// see, keyed by agent id.
//
// Scoped through ListAgents rather than by querying proposals directly, so this
// can never widen what a member is allowed to see: an agent absent from that
// list contributes no id to the query in the first place.
func AgentOutcomeBatch(ctx context.Context, actor Actor) (map[string]*AgentOutcome, error) {
	agents, err := ListAgents(ctx, actor)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(agents))
	for _, a := range agents {
		ids = append(ids, a.Id)
	}
	counts, err := pendingModels.AgentOutcomeCountsBatch(ctx, ids)
	if err != nil {
		return nil, err
	}
	// Every visible agent gets an entry, including one with no proposals yet.
	// A missing key and an unmeasured agent would otherwise be indistinguishable
	// at the call site, and the caller would have to guess which it had.
	out := make(map[string]*AgentOutcome, len(ids))
	for _, id := range ids {
		out[id.String()] = summarize(counts[id.String()])
	}
	return out, nil
}

// AgentOutcomeFor returns one agent's acceptance record, or an unmeasured one
// if the actor cannot see that agent. Never reports "no such agent" by a
// different route than the rest of the package.
func AgentOutcomeFor(ctx context.Context, actor Actor, agentID uuid.UUID) (*AgentOutcome, error) {
	all, err := AgentOutcomeBatch(ctx, actor)
	if err != nil {
		return nil, err
	}
	if o, ok := all[agentID.String()]; ok {
		return o, nil
	}
	return &AgentOutcome{AcceptanceRate: OutcomeUnmeasured}, nil
}
