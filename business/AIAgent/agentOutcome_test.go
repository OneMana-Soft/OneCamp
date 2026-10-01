package business

import (
	"math"
	"testing"

	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
)

func TestSummarizeSeparatesUnmeasuredFromZero(t *testing.T) {
	// The distinction the sentinel exists for. An agent nobody has ruled on and
	// an agent everybody rejected must not render the same.
	unmeasured := summarize(&pendingModels.AgentOutcomeCounts{Pending: 4})
	if unmeasured.AcceptanceRate != OutcomeUnmeasured {
		t.Errorf("nothing decided = %v, want the unmeasured sentinel", unmeasured.AcceptanceRate)
	}
	allRejected := summarize(&pendingModels.AgentOutcomeCounts{Rejected: 4})
	if allRejected.AcceptanceRate != 0 {
		t.Errorf("all rejected = %v, want 0", allRejected.AcceptanceRate)
	}
	if summarize(nil).AcceptanceRate != OutcomeUnmeasured {
		t.Error("nil counts should be unmeasured, not zero")
	}
}

func TestSummarizeCountsAnApprovedFailureAsApproved(t *testing.T) {
	// A person said yes and the execution then broke. That is an execution bug,
	// not a rejected proposal, and blaming the agent's acceptance rate for it
	// would make a working agent look unwanted.
	o := summarize(&pendingModels.AgentOutcomeCounts{Approved: 3, Failed: 1})
	if o.Approved != 3 || o.Failed != 1 {
		t.Fatalf("approved=%d failed=%d, want 3 and 1", o.Approved, o.Failed)
	}
	if o.AcceptanceRate != 1 {
		t.Errorf("acceptance rate = %v, want 1: every proposal was approved", o.AcceptanceRate)
	}
}

func TestSummarizeExcludesUndecidedFromTheRate(t *testing.T) {
	// Expired and pending are not opinions. Counting them as rejections would
	// punish an agent for a person being on holiday.
	o := summarize(&pendingModels.AgentOutcomeCounts{Approved: 3, Rejected: 1, Expired: 6, Pending: 2})
	if o.Decided != 4 {
		t.Errorf("decided = %d, want 4", o.Decided)
	}
	if math.Abs(o.AcceptanceRate-0.75) > 1e-9 {
		t.Errorf("acceptance rate = %v, want 0.75", o.AcceptanceRate)
	}
}

func TestIgnoredNeedsEnoughFinishedProposals(t *testing.T) {
	cases := []struct {
		name string
		in   pendingModels.AgentOutcomeCounts
		want bool
	}{
		// One expiry is 100% of a sample of one and means nothing.
		{"a single expiry proves nothing", pendingModels.AgentOutcomeCounts{Expired: 1}, false},
		{"below the threshold is ordinary noise", pendingModels.AgentOutcomeCounts{Approved: 3, Expired: 1}, false},
		{"mostly ignored", pendingModels.AgentOutcomeCounts{Approved: 1, Expired: 5}, true},
		{"pending is not ignored yet", pendingModels.AgentOutcomeCounts{Pending: 9}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarize(&tc.in).Ignored; got != tc.want {
				t.Errorf("Ignored = %v, want %v", got, tc.want)
			}
		})
	}
}
