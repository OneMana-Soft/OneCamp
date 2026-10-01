package business

// The agent run ledger contributes a section to the evidence pack.
//
// Registered rather than called, for the same reason retention is: the pack is
// assembled in the audit package, which both editions ship, and this ledger
// belongs to the AI packages, which only one does. Linking this package is what
// puts agent runs in the pack; the AI-free edition links nothing here and its
// packs simply have no agent section, which is the truth about that deployment.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

func init() {
	helpers.RegisterEvidenceContributor(helpers.EvidenceSection{
		Name: "agent_runs",
		Describe: "Every agent run started in the window, with the model that read the prompt, a fingerprint of the " +
			"instructions it was given, and which skills were in them at the time. The fingerprints are what make an " +
			"instruction edited later unable to change what a past run appears to have been asked.",
		Collect: func(ctx context.Context, from, to time.Time) (any, error) {
			return model.ListRunsForEvidence(ctx, from, to)
		},
	})
}
